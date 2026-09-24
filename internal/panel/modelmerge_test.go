package panel

import (
	"testing"

	"github.com/4kercc/workbuddy2api-panel/internal/livecfg"
)

// me 构造一个模型条目（测试用最小形状）。
func me(id string, kv map[string]any) map[string]any {
	m := map[string]any{"id": id}
	for k, v := range kv {
		m[k] = v
	}
	return m
}

func TestSplitRealmID(t *testing.T) {
	for _, tc := range []struct{ in, realm, bare string }{
		{"cn:glm-5.3-flash", "cn", "glm-5.3-flash"},
		{"global:glm-5.3-flash", "global", "glm-5.3-flash"},
		{"glm-5.3-flash", "", "glm-5.3-flash"},   // 无前缀
		{"foo:bar", "", "foo:bar"},               // 未知前缀不拆
		{"cn:", "cn", ""},                        // 边界：空前缀尾
		{"", "", ""},                             // 空串
	} {
		realm, bare := splitRealmID(tc.in)
		if realm != tc.realm || bare != tc.bare {
			t.Errorf("splitRealmID(%q) = (%q,%q), want (%q,%q)", tc.in, realm, bare, tc.realm, tc.bare)
		}
	}
}

// TestMergeCrossRealm 同名跨域合并为一条：id 用裸名，realms 列两域，variants 保留
// 每域完整条目（含各自不同的取值）。
func TestMergeCrossRealm(t *testing.T) {
	in := []map[string]any{
		me("cn:glm-5.3-flash", map[string]any{"max_output_tokens": 131072, "credits": "x0.03"}),
		me("global:glm-5.3-flash", map[string]any{"max_output_tokens": 32000, "credits": "x0.00"}),
	}
	out := mergeModelEntries(in)
	if len(out) != 1 {
		t.Fatalf("want 1 merged entry, got %d", len(out))
	}
	m := out[0]
	if m["merged"] != true || m["id"] != "glm-5.3-flash" {
		t.Fatalf("merged=%v id=%v", m["merged"], m["id"])
	}
	realms, _ := m["realms"].([]string)
	if len(realms) != 2 || realms[0] != "cn" || realms[1] != "global" {
		t.Fatalf("realms=%v", realms)
	}
	vs, _ := m["variants"].([]map[string]any)
	if len(vs) != 2 {
		t.Fatalf("variants len=%d", len(vs))
	}
	// 关键：两域各自的取值必须原样保留，不能被"取其一"抹平
	if vs[0]["max_output_tokens"] != 131072 || vs[1]["max_output_tokens"] != 32000 {
		t.Errorf("两域最大输出被抹平: %v / %v", vs[0]["max_output_tokens"], vs[1]["max_output_tokens"])
	}
	if vs[0]["credits"] != "x0.03" || vs[1]["credits"] != "x0.00" {
		t.Errorf("两域积分倍率被抹平: %v / %v", vs[0]["credits"], vs[1]["credits"])
	}
}

// TestMergeKeepsSingleRealm 仅单域存在的模型保持独立条目（id 仍带前缀），不参与合并。
func TestMergeKeepsSingleRealm(t *testing.T) {
	in := []map[string]any{
		me("cn:glm-5.3-flash", nil),
		me("global:glm-5.3-flash", nil),
		me("cn:auto", nil),            // 仅 CN
		me("global:default-model", nil), // 仅 global
	}
	out := mergeModelEntries(in)
	if len(out) != 3 {
		t.Fatalf("want 3 entries (1 merged + 2 singles), got %d", len(out))
	}
	if out[1]["id"] != "cn:auto" || out[1]["merged"] != nil {
		t.Errorf("单域项应原样保留，got %v", out[1]["id"])
	}
	if out[2]["id"] != "global:default-model" {
		t.Errorf("单域项应原样保留，got %v", out[2]["id"])
	}
}

// TestMergeOrderStable 跨域项落在首次出现的位置，单域项原地保留：开关来回切换时
// 行序不变，便于前后对比。
func TestMergeOrderStable(t *testing.T) {
	in := []map[string]any{
		me("cn:a", nil),
		me("global:b", nil),
		me("global:a", nil), // a 的第二次出现（跨域）→ 并入第 0 位
		me("cn:c", nil),
	}
	out := mergeModelEntries(in)
	got := []string{out[0]["id"].(string), out[1]["id"].(string), out[2]["id"].(string)}
	want := []string{"a", "global:b", "cn:c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v, want %v", got, want)
		}
	}
}

// TestMergeEmpty 空输入返回非 nil 空切片（JSON 序列化为 [] 而非 null）。
func TestMergeEmpty(t *testing.T) {
	out := mergeModelEntries(nil)
	if out == nil {
		t.Fatal("want non-nil empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("len=%d", len(out))
	}
}

// TestModelMergeEnabled 开关读 Live 快照（热生效）；无 Live 视为关闭。
func TestModelMergeEnabled(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	if p.modelMergeEnabled() {
		t.Error("无 Live 应视为关闭")
	}

	live := livecfg.New(livecfg.Snapshot{PanelModelMerge: true})
	p2 := New(Config{Version: "test", APIKey: "k", Live: live})
	if !p2.modelMergeEnabled() {
		t.Error("Live 开启时应报告开启")
	}

	// 面板保存后替换快照 → 立即生效（无需重启）
	live.Store(livecfg.Snapshot{PanelModelMerge: false})
	if p2.modelMergeEnabled() {
		t.Error("热关闭后应报告关闭")
	}
}
