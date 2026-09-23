package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// promptResp 面板 /panel/api/prompt 的响应形状。
type promptResp struct {
	OK       bool   `json:"ok"`
	Mode     string `json:"mode"`
	Text     string `json:"text"`
	Source   string `json:"source"`
	Injected bool   `json:"injected"`
}

func getPrompt(t *testing.T, p *Panel, withKey bool) (*httptest.ResponseRecorder, promptResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/prompt", nil)
	if withKey {
		req.Header.Set("Authorization", "Bearer test-key")
	}
	p.ServeHTTP(rec, req)

	var got promptResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, got
}

// TestPromptInfoReportsEffective 面板须能读到"当前实际注入什么"及其来源——
// 这是用户"看看如何优化"的前提：看不到生效内容就无从改起。
func TestPromptInfoReportsEffective(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "test-key",
		PromptInfo: func() PromptSnapshot {
			return PromptSnapshot{Mode: "custom", Text: "你是助手。", Source: "inline"}
		},
	})
	rec, got := getPrompt(t, p, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !got.OK || got.Mode != "custom" || got.Text != "你是助手。" || got.Source != "inline" {
		t.Fatalf("got %+v", got)
	}
	if !got.Injected {
		t.Error("custom + 非空文本应报告 injected=true")
	}
}

// TestPromptInfoPassthroughNotInjected passthrough 下不注入：injected=false，
// 面板据此提示"当前不注入"，避免用户以为填了内容就生效。
func TestPromptInfoPassthroughNotInjected(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "test-key",
		PromptInfo: func() PromptSnapshot {
			return PromptSnapshot{Mode: "passthrough", Source: "none"}
		},
	})
	_, got := getPrompt(t, p, true)
	if got.Injected {
		t.Error("passthrough 应报告 injected=false")
	}
	if got.Mode != "passthrough" || got.Source != "none" {
		t.Fatalf("got %+v", got)
	}
}

// TestPromptInfoEmptyModeFallsBack passthrough 是空 mode 的归一化结果，
// 零值快照（未注入 PromptInfo 的裁剪部署）也须给出可读的 mode 而非空串。
func TestPromptInfoEmptyModeFallsBack(t *testing.T) {
	p := New(Config{
		Version:    "test",
		APIKey:     "test-key",
		PromptInfo: func() PromptSnapshot { return PromptSnapshot{} },
	})
	_, got := getPrompt(t, p, true)
	if got.Mode != "passthrough" || got.Source != "none" || got.Injected {
		t.Fatalf("零值快照应回落 passthrough/none/不注入，got %+v", got)
	}
}

// TestPromptInfoUnavailable PromptInfo 未注入 → 501（而非空内容误导用户）。
func TestPromptInfoUnavailable(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	rec, _ := getPrompt(t, p, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501", rec.Code)
	}
}

// TestPromptInfoRequiresAuth 端点复用网关 api_key 鉴权。
func TestPromptInfoRequiresAuth(t *testing.T) {
	p := New(Config{
		Version:    "test",
		APIKey:     "test-key",
		PromptInfo: func() PromptSnapshot { return PromptSnapshot{Mode: "custom", Text: "x"} },
	})
	rec, _ := getPrompt(t, p, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}
