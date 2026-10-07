package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
)

// TestEarnedTodayTracksAuthoritativeGain 「今日新增积分」的基本口径：
// 首次观测只建基准，之后的权威余额正向增量累加，消耗（余额下降）不抵减。
func TestEarnedTodayTracksAuthoritativeGain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 新账号 credits=0，第一次刷新拿到 1000：这是"已有余额"，不是"今天赚的"。
	p.SetCredits("u1", 1000, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 0 {
		t.Fatalf("首次观测 EarnedToday=%d want 0（只建基准不记账）", st.EarnedToday)
	}

	// 签到到账 +300。
	p.SetCredits("u1", 1300, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 300 {
		t.Fatalf("EarnedToday=%d want 300", st.EarnedToday)
	}

	// 消耗把余额打到 900：新增仍记 300（问的是"赚了多少"，不是净变化）。
	p.SetCredits("u1", 900, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 300 {
		t.Fatalf("消耗后 EarnedToday=%d want 300（消耗不抵减）", st.EarnedToday)
	}

	// 再赚 +100（900 → 1000）。
	p.SetCredits("u1", 1000, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 400 {
		t.Fatalf("EarnedToday=%d want 400", st.EarnedToday)
	}
}

// TestEarnedTodayIgnoresLocalDeduction NoteModelCost 的本地扣减不得污染台账：
// 它只是两次刷新之间的内插估计，随后的权威刷新把值"修正回来"不是新增。
// 否则每次聊天后的余额刷新都会虚增一笔，今日新增会变成消耗量的镜像。
func TestEarnedTodayIgnoresLocalDeduction(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0) // 建基准

	p.NoteModelCost("u1", "glm-5.2", 40, 1000) // 本地扣 40
	if st, _ := p.Status("u1"); st.Credits != 960 {
		t.Fatalf("credits=%d want 960（本地扣减应生效）", st.Credits)
	}
	// 上游权威值仍是 1000（尚未扣账）→ 刷新把 960 修正回 1000。
	p.SetCredits("u1", 1000, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 0 {
		t.Fatalf("EarnedToday=%d want 0（本地扣减被修正不得记成新增）", st.EarnedToday)
	}
}

// TestEarnedTodayResetsOnNewDay 跨天：日键不是今天就归零重新累计，
// 且昨天的累计不得冒充今天（跨天后到首次刷新之间内存里还留着旧值）。
func TestEarnedTodayResetsOnNewDay(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u1", 1500, 0) // 今日 +500

	// 把日键推到昨天（模拟进程持续运行跨过零点，台账还停在前一天）。
	p.mu.Lock()
	p.byUID["u1"].earnedDay = time.Now().AddDate(0, 0, -1).Format(creditDayLayout)
	p.mu.Unlock()

	if st, _ := p.Status("u1"); st.EarnedToday != 0 {
		t.Fatalf("跨天后 EarnedToday=%d want 0（昨天的值不得冒充今天）", st.EarnedToday)
	}

	// 今天第一次权威刷新：归零重新累计，基准仍是上次观测（1500）。
	p.SetCredits("u1", 1700, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 200 {
		t.Fatalf("新的一天 EarnedToday=%d want 200（相对上次权威观测的增量）", st.EarnedToday)
	}
}

// TestEarnedTodayPersistsAcrossReload 台账落盘：重启不能让当天已赚的积分凭空消失。
func TestEarnedTodayPersistsAcrossReload(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u1", 1600, 0) // 今日 +600
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"credits_auth"`, `"earned_today"`, `"earned_day"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("reload 后账号应存在")
	}
	if st.EarnedToday != 600 {
		t.Fatalf("reload 后 EarnedToday=%d want 600", st.EarnedToday)
	}
	// 基准也一并恢复：重启后相对旧基准的增量继续累加（600 + 400）。
	p2.SetCredits("u1", 2000, 0)
	if st, _ := p2.Status("u1"); st.EarnedToday != 1000 {
		t.Fatalf("reload 后继续累计 EarnedToday=%d want 1000", st.EarnedToday)
	}
}

// TestEarnedTodayZeroWhenNeverObserved 从未有过权威观测（加号但没刷新过）时透出 0，
// 不是"未知"——面板求和口径下 0 与缺失等价，但字段本身必须存在且为 0。
func TestEarnedTodayZeroWhenNeverObserved(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if st.EarnedToday != 0 {
		t.Fatalf("EarnedToday=%d want 0", st.EarnedToday)
	}
	// 从未观测 → 不能因为"第一次刷新"就记账（基准尚未建立）。
	p.SetCredits("u1", 500, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 0 {
		t.Fatalf("首次刷新后 EarnedToday=%d want 0", st.EarnedToday)
	}
}

// TestEarnedTodaySetCreditsDetailed 走 SetCreditsDetailed（签到/余额刷新主路径）
// 与 SetCredits 同一台账，不能因为换入口就漏记。
func TestEarnedTodaySetCreditsDetailed(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 800, 1000, 100)
	p.SetCreditsDetailed("u1", 900, 1000, 0)
	if st, _ := p.Status("u1"); st.EarnedToday != 100 {
		t.Fatalf("EarnedToday=%d want 100", st.EarnedToday)
	}
}

// TestEarnedTodayPerAccountIndependent 台账按账号独立：一个账号的消耗不得影响
// 另一个账号的新增（面板按账号求和，串号会直接算错总数）。
func TestEarnedTodayPerAccountIndependent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 100, 0)
	p.SetCredits("u1", 300, 0) // u1 +200
	p.SetCredits("u2", 50, 0)  // u2 纯消耗

	var sum int64
	for _, st := range p.List() {
		sum += st.EarnedToday
	}
	if sum != 200 {
		t.Fatalf("池内今日新增合计=%d want 200", sum)
	}
}

// TestEarnedTodayOldStateFileNoFakeGain 向后兼容：旧版 state.json 没有
// credits_auth/earned_* 字段（零值 = 基准未建立）。升级后第一次余额刷新不得把
// 整份余额记成"今天新增"——老用户的号都是满额的，误记会直接顶出五位数假新增。
func TestEarnedTodayOldStateFileNoFakeGain(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	old := `{"accounts":{"u1":{"credits":36000,"credits_total":36072}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.Credits != 36000 {
		t.Fatalf("credits=%d want 36000（旧文件余额应恢复）", st.Credits)
	}

	// 升级后第一次刷新（余额没变）：只建基准，不记账。
	p.SetCredits("u1", 36000, 36072)
	if st, _ := p.Status("u1"); st.EarnedToday != 0 {
		t.Fatalf("EarnedToday=%d want 0（旧文件无基准，首次刷新只建基准）", st.EarnedToday)
	}
	// 之后的增量正常累计。
	p.SetCredits("u1", 36200, 36072)
	if st, _ := p.Status("u1"); st.EarnedToday != 200 {
		t.Fatalf("EarnedToday=%d want 200", st.EarnedToday)
	}
}
