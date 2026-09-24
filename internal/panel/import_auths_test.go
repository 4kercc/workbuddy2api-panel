package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
	"github.com/4kercc/workbuddy2api-panel/internal/pool"
)

// testPanelWithPool 建一个带真实 Pool 的面板（Upstream 留 nil → 签到等附属动作跳过，
// 测试不触网）。AuthDir 指向临时目录，落盘结果可直接断言。
func testPanelWithPool(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("mkdir auths: %v", err)
	}
	p := New(Config{
		Version: "test",
		APIKey:  "test-key",
		Pool:    pool.New(filepath.Join(dir, "state.json")),
		AuthDir: authDir,
	})
	return p, authDir
}

// multipartUpload 把 name→内容 组装成 files 字段的 multipart 请求体。
func multipartUpload(t *testing.T, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, body := range files {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// 本地部署 auths/ 目录里的真实形态：嵌套形（CN / Global 各一）与扁平形。
const (
	nestedCN = `{"auth":{"accessToken":"at-cn","refreshToken":"rt-cn","expiresAt":4102444800,` +
		`"domain":"www.codebuddy.cn","realm":"cn"},` +
		`"account":{"uid":"uid-cn","enterpriseId":"ent-cn","nickname":"nick-cn"}}`
	nestedGlobal = `{"auth":{"accessToken":"at-gl","refreshToken":"rt-gl","expiresAt":4102444800,` +
		`"domain":"www.workbuddy.ai","realm":"global"},` +
		`"account":{"uid":"uid-gl","enterpriseId":"ent-gl","nickname":"nick-gl"}}`
	flatCN = `{"accessToken":"at-flat","refreshToken":"rt-flat","expiresAt":4102444800,` +
		`"domain":"www.codebuddy.cn","realm":"cn","uid":"uid-flat","nickname":"nick-flat"}`
	// state.json 形态（目录里必然混入的兄弟文件）：无 accessToken，必须被识别为非凭证。
	stateJSONLike = `{"accounts":{"uid-cn":{"credits":100,"disabled":false}}}`
)

func TestParseAuthBlobForms(t *testing.T) {
	t.Run("嵌套形", func(t *testing.T) {
		got, err := parseAuthBlob([]byte(nestedCN))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 account, got %d", len(got))
		}
		if got[0].UID != "uid-cn" || got[0].RealmStored() != "cn" || got[0].Nickname != "nick-cn" {
			t.Fatalf("parsed = uid:%q realm:%q nick:%q", got[0].UID, got[0].RealmStored(), got[0].Nickname)
		}
	})

	t.Run("扁平形", func(t *testing.T) {
		got, err := parseAuthBlob([]byte(flatCN))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].UID != "uid-flat" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("数组形态跳过坏元素", func(t *testing.T) {
		blob := `[` + nestedCN + `,{"nope":1},` + flatCN + `]`
		got, err := parseAuthBlob([]byte(blob))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 valid accounts, got %d", len(got))
		}
		if got[0].UID != "uid-cn" || got[1].UID != "uid-flat" {
			t.Fatalf("got uid %q / %q", got[0].UID, got[1].UID)
		}
	})

	t.Run("非凭证文件报错", func(t *testing.T) {
		if _, err := parseAuthBlob([]byte(stateJSONLike)); err == nil {
			t.Fatal("state.json 形态应报错，供调用方静默跳过")
		}
	})

	t.Run("空文件报错", func(t *testing.T) {
		if _, err := parseAuthBlob([]byte(" \n\t ")); err == nil {
			t.Fatal("空文件应报错")
		}
	})
}

// TestImportAuthsEndToEnd 走完整 HTTP 路径：多文件上传 → 落盘 → 进池。
// 目录里混入 state.json 应被静默跳过，不污染 imported/errors。
func TestImportAuthsEndToEnd(t *testing.T) {
	p, authDir := testPanelWithPool(t)
	body, ct := multipartUpload(t, map[string]string{
		"workbuddy-uid-cn.json": nestedCN,
		"workbuddy-uid-gl.json": nestedGlobal,
		"state.json":            stateJSONLike,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/import/auths", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK       bool     `json:"ok"`
		Files    int      `json:"files"`
		Total    int      `json:"total"`
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.OK || got.Files != 3 || got.Total != 2 || got.Imported != 2 || got.Skipped != 1 {
		t.Fatalf("counts = ok:%v files:%d total:%d imported:%d skipped:%d (want true/3/2/2/1); errors=%v",
			got.OK, got.Files, got.Total, got.Imported, got.Skipped, got.Errors)
	}
	if len(got.Errors) != 0 {
		t.Errorf("非凭证文件不该产生错误条目: %v", got.Errors)
	}

	// 凭证落盘且能原样读回（realm 标识随文件内容保留，不被 domain 推断覆盖）。
	for _, tc := range []struct{ uid, wantRealm, wantNick string }{
		{"uid-cn", "cn", "nick-cn"},
		{"uid-gl", "global", "nick-gl"},
	} {
		path := filepath.Join(authDir, "workbuddy-"+tc.uid+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if a.UID != tc.uid || a.RealmStored() != tc.wantRealm || a.Nickname != tc.wantNick {
			t.Errorf("%s: uid=%q realm=%q nick=%q, want %q/%q/%q",
				path, a.UID, a.RealmStored(), a.Nickname, tc.uid, tc.wantRealm, tc.wantNick)
		}
	}

	// 只应落盘两个凭证文件（state.json 不得变成凭证）。
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read authDir: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("authDir 文件数 = %d %v, want 2", len(entries), names)
	}
}

// TestImportAuthsRejectsPathTraversalUID uid 来自文件内容又用于拼文件名，
// 含路径字符的 uid 必须被白名单拒绝，且不产生任何落盘文件。
func TestImportAuthsRejectsPathTraversalUID(t *testing.T) {
	p, authDir := testPanelWithPool(t)
	evil := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":4102444800,` +
		`"domain":"www.codebuddy.cn"},"account":{"uid":"../../../etc/pwn"}}`
	body, ct := multipartUpload(t, map[string]string{"workbuddy-evil.json": evil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/import/auths", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)

	var got struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Imported != 0 || got.Skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want 0/1; errors=%v", got.Imported, got.Skipped, got.Errors)
	}
	if entries, _ := os.ReadDir(authDir); len(entries) != 0 {
		t.Errorf("authDir 出现 %d 个文件，路径穿越未被拦截", len(entries))
	}
}

// TestImportAuthsRequiresAuth 导入端点复用网关 api_key 鉴权：无密钥必须 401。
func TestImportAuthsRequiresAuth(t *testing.T) {
	p, _ := testPanelWithPool(t)
	body, ct := multipartUpload(t, map[string]string{"workbuddy-uid-cn.json": nestedCN})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/import/auths", body)
	req.Header.Set("Content-Type", ct)
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
}

// TestImportAuthsMissingField 未带 files 字段时返回 400（而不是 500）。
func TestImportAuthsMissingField(t *testing.T) {
	p, _ := testPanelWithPool(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/import/auths", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

// TestNormalizeStoredRealm 脏值/大小写/空白都必须归一化到 cn/global/""。
func TestNormalizeStoredRealm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"cn", "cn"},
		{"CN", "cn"},
		{"  global  ", "global"},
		{"Global", "global"},
		{"", ""},
		{"bogus", ""},
		{"workbuddy.ai", ""},
	} {
		if got := normalizeStoredRealm(tc.in); got != tc.want {
			t.Errorf("normalizeStoredRealm(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
