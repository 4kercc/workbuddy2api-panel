package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseAuthBlobBundle 「导出配置」产物（{"accounts":[...]}）能被导入侧解析，
// 且不与单个 auth 对象（无 accounts 键）混淆。
func TestParseAuthBlobBundle(t *testing.T) {
	bundle := `{"app":"workbuddy2api-panel","version":"test","exported_at":"2026-10-27T00:00:00Z",` +
		`"accounts":[` + nestedCN + `,` + nestedGlobal + `],"config":{"server":{"port":7863}}}`

	accs, err := parseAuthBlob([]byte(bundle))
	if err != nil {
		t.Fatalf("bundle 解析失败: %v", err)
	}
	if len(accs) != 2 {
		t.Fatalf("bundle 账号数 = %d, want 2", len(accs))
	}
	if accs[0].UID != "uid-cn" || accs[1].UID != "uid-gl" {
		t.Errorf("uid = %q/%q, want uid-cn/uid-gl", accs[0].UID, accs[1].UID)
	}

	// 单个 auth 对象不得被误判为备份包（无 accounts 键 → 走单对象解析）。
	single, err := parseAuthBlob([]byte(nestedCN))
	if err != nil || len(single) != 1 || single[0].UID != "uid-cn" {
		t.Fatalf("单对象解析 = %v err=%v, want 1 个 uid-cn", single, err)
	}

	// 空 accounts 数组不是合法备份包（解析不出账号 → 报错，由调用方按非凭证文件跳过）。
	if _, err := parseAuthBlob([]byte(`{"accounts":[],"config":{}}`)); err == nil {
		t.Errorf("空 accounts 应报错，实际 nil")
	}
}

// TestExportConfigEndToEnd 导出端点返回可下载的备份包（accounts + config），
// 且导出的字节可原样被 parseAuthBlob 重新导入（迁移往返无损）。
func TestExportConfigEndToEnd(t *testing.T) {
	p, authDir := testPanelWithPool(t)
	p.cfg.LoadConfig = func() (any, error) {
		return map[string]any{"server": map[string]any{"port": 7863}}, nil
	}
	// 两个真实凭证文件 + 一个目录里必然混入的非凭证文件（state.json，必须被忽略）。
	for name, content := range map[string]string{
		"workbuddy-uid-cn.json": nestedCN,
		"workbuddy-uid-gl.json": nestedGlobal,
		"state.json":            stateJSONLike,
	} {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/export", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment 下载头", cd)
	}

	var got exportBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if got.App != "workbuddy2api-panel" {
		t.Errorf("app = %q", got.App)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2（state.json 必须被忽略）", len(got.Accounts))
	}
	if got.Config == nil {
		t.Errorf("config 缺失（LoadConfig 已注入）")
	}

	// 往返：导出的字节直接喂回 parseAuthBlob（模拟「导出→在另一实例导入」）。
	accs, err := parseAuthBlob(rec.Body.Bytes())
	if err != nil || len(accs) != 2 {
		t.Fatalf("导出字节重新解析失败: n=%d err=%v", len(accs), err)
	}
}

// TestExportConfigRequiresAuth 未带密钥时 401。
func TestExportConfigRequiresAuth(t *testing.T) {
	p, _ := testPanelWithPool(t)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/export", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
}
