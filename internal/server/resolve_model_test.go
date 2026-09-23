package server

import "testing"

func TestResolveModelEx(t *testing.T) {
	for _, tc := range []struct {
		in       string
		realm    string
		bare     string
		explicit bool
	}{
		{"cn:glm-5.3-flash", "cn", "glm-5.3-flash", true},
		{"global:glm-5.3-flash", "global", "glm-5.3-flash", true},
		{"glm-5.3-flash", "cn", "glm-5.3-flash", false},
		{"foo:bar", "cn", "foo:bar", false}, // 未知前缀不拆，整体视为裸名
		{"CN:glm", "cn", "CN:glm", false},   // 大小写敏感
		{"cn:", "cn", "", true},
		{"", "cn", "", false},
	} {
		realm, bare, explicit := resolveModelEx(tc.in)
		if realm != tc.realm || bare != tc.bare || explicit != tc.explicit {
			t.Errorf("resolveModelEx(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, realm, bare, explicit, tc.realm, tc.bare, tc.explicit)
		}
	}
}

// TestResolveRouteCrossRealm 跨域白名单的核心语义：
// 裸名命中 → realm=""（池内 = 不做域过滤）；显式前缀恒锁域，不被白名单覆盖。
func TestResolveRouteCrossRealm(t *testing.T) {
	cross := []string{"deepseek-v4.1-flash"}
	for _, tc := range []struct {
		name, in, realm, bare string
	}{
		{"裸名命中白名单 → 不过滤", "deepseek-v4.1-flash", "", "deepseek-v4.1-flash"},
		{"裸名未命中 → cn", "glm-5.3-flash", "cn", "glm-5.3-flash"},
		{"显式 cn 前缀不被白名单覆盖", "cn:deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"显式 global 前缀不被白名单覆盖", "global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash"},
		{"白名单里的大小写不匹配 → cn", "Deepseek-V4.1-Flash", "cn", "Deepseek-V4.1-Flash"},
	} {
		realm, bare := ResolveRoute(tc.in, cross)
		if realm != tc.realm || bare != tc.bare {
			t.Errorf("%s: ResolveRoute(%q) = (%q,%q), want (%q,%q)",
				tc.name, tc.in, realm, bare, tc.realm, tc.bare)
		}
	}
}

// TestResolveRouteEmptyListZeroRegression 白名单为空（缺省配置）时，行为必须与
// 只做前缀解析完全一致——老部署升级后选号域不能有任何变化。
func TestResolveRouteEmptyListZeroRegression(t *testing.T) {
	for _, tc := range []struct{ in, realm, bare string }{
		{"deepseek-v4.1-flash", "cn", "deepseek-v4.1-flash"},
		{"cn:x", "cn", "x"},
		{"global:x", "global", "x"},
		{"foo:bar", "cn", "foo:bar"},
		{"", "cn", ""},
	} {
		for _, cross := range [][]string{nil, {}} {
			realm, bare := ResolveRoute(tc.in, cross)
			if realm != tc.realm || bare != tc.bare {
				t.Errorf("ResolveRoute(%q, %v) = (%q,%q), want (%q,%q)",
					tc.in, cross, realm, bare, tc.realm, tc.bare)
			}
		}
	}
}

// TestResolveRouteBareNameNotBlankRealm 反证：白名单存在但模型不匹配时仍必须给 cn，
// 绝不能让"白名单非空"本身导致域过滤被误关（那会让所有裸名请求都可能打到 global 号）。
func TestResolveRouteBareNameNotBlankRealm(t *testing.T) {
	cross := []string{"other-model"}
	realm, bare := ResolveRoute("deepseek-v4.1-flash", cross)
	if realm != "cn" {
		t.Fatalf("白名单未命中时 realm=%q, want cn", realm)
	}
	if bare != "deepseek-v4.1-flash" {
		t.Fatalf("bare=%q", bare)
	}
}
