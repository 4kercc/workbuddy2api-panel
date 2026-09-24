package server

import (
	"testing"

	"github.com/4kercc/workbuddy2api-panel/internal/livecfg"
)

// TestPromptCfgPrefersLive 面板改提示词必须立即生效：promptCfg 优先读 Live 快照。
//
// 回归背景：PromptMode/PromptText 此前是 handler 的启动期静态字段，既不在 livecfg
// 快照里、也没被 saveConfig 应用，且 restartRequiredFields 未列 prompt —— 面板保存
// 报成功但出站注入的仍是旧提示词，且不提示需要重启（静默失效）。
func TestPromptCfgPrefersLive(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{PromptMode: "custom", PromptText: "快照里的提示词"})
	h := NewHandler(Config{PromptMode: "passthrough", Live: live})

	mode, text := h.promptCfg()
	if mode != "custom" || text != "快照里的提示词" {
		t.Fatalf("got (%q,%q) want (custom, 快照里的提示词)", mode, text)
	}

	// 模拟面板保存：替换快照后，下一次请求即读到新值（热生效，无需重启）。
	live.Store(livecfg.Snapshot{PromptMode: "append", PromptText: "改过的提示词"})
	mode, text = h.promptCfg()
	if mode != "append" || text != "改过的提示词" {
		t.Fatalf("热更新后 got (%q,%q) want (append, 改过的提示词)", mode, text)
	}
}

// TestPromptCfgFallsBackToStatic 快照未携带提示词字段时（只装了 api_key 的场景）
// 整体回落静态值：若按空串判 mode，注入会被静默关闭。
func TestPromptCfgFallsBackToStatic(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "k"}) // 未设 PromptMode
	h := NewHandler(Config{PromptMode: "custom", PromptText: "静态提示词", Live: live})

	mode, text := h.promptCfg()
	if mode != "custom" || text != "静态提示词" {
		t.Fatalf("got (%q,%q) want (custom, 静态提示词)", mode, text)
	}
}

// TestPromptCfgNoLive 无 Live（测试/裸用）时用静态字段。
func TestPromptCfgNoLive(t *testing.T) {
	h := NewHandler(Config{PromptMode: "custom", PromptText: "静态"})
	mode, text := h.promptCfg()
	if mode != "custom" || text != "静态" {
		t.Fatalf("got (%q,%q) want (custom, 静态)", mode, text)
	}
}

// TestPromptCfgLiveOverridesToPassthrough 快照显式切 passthrough 时必须能关掉注入
// （用户从 custom 切回 passthrough 是合法操作，不能被静态值粘住）。
func TestPromptCfgLiveOverridesToPassthrough(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{PromptMode: "passthrough"})
	h := NewHandler(Config{PromptMode: "custom", PromptText: "旧的静态提示词", Live: live})

	mode, text := h.promptCfg()
	if mode != "passthrough" || text != "" {
		t.Fatalf("got (%q,%q) want (passthrough, 空)", mode, text)
	}
}
