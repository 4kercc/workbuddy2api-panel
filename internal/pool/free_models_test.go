package pool

import (
	"testing"
	"time"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
)

// freeOf 在锁内读 isFreeModel（生产代码里它只在持锁路径被调用）。
func freeOf(p *Pool, realm, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isFreeModel(realm, model)
}

// newGlobalPool 构造空池并确保 globalEnabled 开启（其它用例可能关过它，
// 关掉后 Domain=workbuddy.ai 的账号会被降级成 cn，导致断言随机失败）。
func newGlobalPool(t *testing.T) *Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	return New("")
}

// TestSetFreeModelsMatching 覆盖白名单的归一化与匹配口径：
//   - "realm:model" 只匹配该域；"model" 匹配所有域；
//   - 大小写/前后空白一律归一（配置写成 HY3 也能命中）；
//   - 空模型名（无模型上下文的调用）恒不豁免。
func TestSetFreeModelsMatching(t *testing.T) {
	p := New("")
	p.SetFreeModels([]string{" global:hy3 ", "HY4FREE", "cn:glm-x", "", "  "})

	cases := []struct {
		realm, model string
		want         bool
	}{
		{"global", "hy3", true},
		{"global", "HY3", true}, // 大小写归一
		{"cn", "hy3", false},    // 域不匹配
		{"global", "hy4free", true},
		{"cn", "hy4free", true}, // 不限定域
		{"cn", "glm-x", true},
		{"global", "glm-x", false}, // 域不匹配
		{"global", "", false},      // 空模型不豁免
		{"global", "deepseek-v4.1-flash", false},
	}
	for _, c := range cases {
		if got := freeOf(p, c.realm, c.model); got != c.want {
			t.Errorf("isFreeModel(%q,%q)=%v want %v", c.realm, c.model, got, c.want)
		}
	}

	// 空配置 = 关闭豁免（零回归）。
	p.SetFreeModels(nil)
	if freeOf(p, "global", "hy3") {
		t.Error("empty free_models should disable exemption")
	}
}

// TestReserveBlockedFreeModelExempt 保留积分停牌对免费模型豁免、对付费模型照旧停牌。
func TestReserveBlockedFreeModelExempt(t *testing.T) {
	p := newGlobalPool(t)
	p.SetReserveCredits(10)
	p.SetFreeModels([]string{"global:hy3"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 0, 100) // 余额 0 / 总额 100 → 低于阈值

	p.mu.RLock()
	e := p.byUID["g1"]
	paidBlocked := p.reserveBlocked(e, "deepseek-v4.1-flash")
	freeBlocked := p.reserveBlocked(e, "hy3")
	noModelBlocked := p.reserveBlocked(e, "")
	p.mu.RUnlock()

	if !paidBlocked {
		t.Error("付费模型应被保留积分拦下")
	}
	if freeBlocked {
		t.Error("免费模型不应被保留积分拦下")
	}
	if !noModelBlocked {
		t.Error("无模型上下文（状态查询）应按停牌处理")
	}

	// 阈值 0 = 关闭：付费模型也不再拦。
	p.SetReserveCredits(0)
	p.mu.RLock()
	paidBlocked = p.reserveBlocked(p.byUID["g1"], "deepseek-v4.1-flash")
	p.mu.RUnlock()
	if paidBlocked {
		t.Error("reserve_credits=0 应关闭停牌")
	}
}

// TestHealthyForModelFreeExemptsHardCreditCooldown 免费模型只豁免 CoolHard（余额不足），
// CoolSoft（限流）与熔断/禁用照旧拦截——豁免不能扩散成"一切惩罚都免"。
func TestHealthyForModelFreeExemptsHardCreditCooldown(t *testing.T) {
	p := newGlobalPool(t)
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Cooldown("g1", CoolHard, time.Hour, "余额不足")

	now := time.Now()
	p.mu.RLock()
	e := p.byUID["g1"]
	paidOK := e.healthyForModelEx(now, "deepseek-v4.1-flash", false)
	freeOK := e.healthyForModelEx(now, "hy3", true)
	legacyOK := e.healthyForModel(now, "hy3") // 不带豁免的旧口径
	p.mu.RUnlock()

	if paidOK {
		t.Error("付费模型在 CoolHard 冷却中应不可选")
	}
	if !freeOK {
		t.Error("免费模型应豁免 CoolHard 冷却")
	}
	if legacyOK {
		t.Error("不带豁免的 healthyForModel 不应放行 CoolHard")
	}

	// CoolSoft 不在豁免范围（限流与余额无关）。
	p.Cooldown("g1", CoolSoft, time.Hour, "429 rate limit")
	p.mu.RLock()
	freeOK = p.byUID["g1"].healthyForModelEx(time.Now(), "hy3", true)
	p.mu.RUnlock()
	if freeOK {
		t.Error("CoolSoft 限流不应被免费模型豁免")
	}

	// 熔断不在豁免范围。
	p.Cooldown("g1", CoolSoft, -time.Second, "429 rate limit") // 提前解除冷却
	p.mu.Lock()
	p.byUID["g1"].breakerUntil = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.mu.RLock()
	freeOK = p.byUID["g1"].healthyForModelEx(time.Now(), "hy3", true)
	p.mu.RUnlock()
	if freeOK {
		t.Error("熔断不应被免费模型豁免")
	}
}

// TestPickFreeModelUsesZeroBalanceAccount 端到端选号口径：
// 0 余额 + 保留积分停牌时，付费模型选不到号，免费模型能选中该账号。
func TestPickFreeModelUsesZeroBalanceAccount(t *testing.T) {
	withNoPickGap(t)
	p := newGlobalPool(t)
	p.SetReserveCredits(10)
	p.SetFreeModels([]string{"global:hy3"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 0, 100)

	if got := p.pick(nil, "deepseek-v4.1-flash", "global"); got != nil {
		t.Fatalf("付费模型不应选中 0 余额号，got %s", got.UID)
	}
	got := p.pick(nil, "hy3", "global")
	if got == nil || got.UID != "g1" {
		t.Fatalf("免费模型应选中 0 余额号，got %+v", got)
	}

	// 关闭阈值后付费模型恢复可选（证明拦截确由保留积分造成，而非其它状态）。
	p.SetReserveCredits(0)
	if got := p.pick(nil, "deepseek-v4.1-flash", "global"); got == nil {
		t.Fatal("reserve_credits=0 时付费模型应可选中")
	}
}

// TestPickForProbeIgnoresReserveAndHardCooldown 目录探测专属入口：
// 0 余额 + CoolHard 时业务选号返回 nil，PickForProbe 仍能拿到号；且探测不动 LRU 序号。
func TestPickForProbeIgnoresReserveAndHardCooldown(t *testing.T) {
	p := newGlobalPool(t)
	p.SetReserveCredits(10)
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 0, 100)
	p.Cooldown("g1", CoolHard, time.Hour, "余额不足")

	if got := p.Pick(); got != nil {
		t.Fatalf("业务选号不应选中 0 余额冷却号，got %s", got.UID)
	}
	got := p.PickForProbe("global")
	if got == nil || got.UID != "g1" {
		t.Fatalf("PickForProbe(global) 应返回 0 余额号，got %+v", got)
	}
	if got := p.PickForProbe("cn"); got != nil {
		t.Fatalf("PickForProbe(cn) 不应跨域返回 global 号，got %s", got.UID)
	}

	p.mu.RLock()
	seq := p.byUID["g1"].usedSeq
	p.mu.RUnlock()
	if seq != 0 {
		t.Errorf("探测不应推进 usedSeq，got %d", seq)
	}

	// 无账号的域返回 nil。
	if got := p.PickForProbe(""); got == nil {
		t.Error("realm 为空表示不过滤，应返回账号")
	}
}
