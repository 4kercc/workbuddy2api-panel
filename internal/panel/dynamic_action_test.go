package panel

import (
	"strings"
	"testing"

	"github.com/4kercc/workbuddy2api-panel/internal/upstream"
)

// TestExpertNameFromTask 动态适配的识别规则（L2）。用例全部取自 2026-10-30 上游
// 真实任务文案——识别错了要么白跑一次真实对话（花积分），要么把公众号/主题任务
// 当成专家任务去搜市场。
func TestExpertNameFromTask(t *testing.T) {
	cases := []struct {
		label string
		task  upstream.Task
		want  string
	}{
		{
			label: "desc 标准形态",
			task:  upstream.Task{TaskDesc: "在专家页搜索并召唤「抖音策略师」，完成一次对话。"},
			want:  "抖音策略师",
		},
		{
			label: "desc 带活动说明前缀",
			task:  upstream.Task{TaskDesc: "即日起至11月15日，在专家页搜索并召唤「无畏契约拼豆专家」，完成一次对话。"},
			want:  "无畏契约拼豆专家",
		},
		{
			label: "desc 为空时走 title 兜底（专家在括号外）",
			task:  upstream.Task{Title: "体验「腾讯轻量云」专家"},
			want:  "腾讯轻量云",
		},
		{
			label: "desc 优先于 title",
			task:  upstream.Task{
				Title:    "体验「某个别的名字」专家",
				TaskDesc: "在专家页搜索并召唤「抖音策略师」，完成一次对话。",
			},
			want:  "抖音策略师",
		},
		{
			// 标题是「关注「腾讯 WorkBuddy」官方公众号」——裸「X」模式会把公众号名当专家名。
			label: "关注公众号任务不得误判",
			task:  upstream.Task{
				Title:    "关注「腾讯 WorkBuddy」官方公众号",
				TaskDesc: "关注「腾讯 WorkBuddy」官方公众号满 24 小时，即可领取奖励。",
			},
			want:  "",
		},
		{
			// 主题任务标题同为「体验「X」」形态，但括号后是「主题」不是「专家」。
			label: "主题任务不得误判",
			task:  upstream.Task{TaskCode: "Hp_Appearance", Title: "体验「和平精英」主题"},
			want:  "",
		},
		{
			// 通用专家任务文案里「专家」在括号内且无「召唤「X」」形态。
			label: "通用专家任务文案不得误判",
			task:  upstream.Task{TaskDesc: "前往WorkBuddy，进入「专家」，召唤你所需领域的专家，每召唤一位专家并发起对话计 1 次。"},
			want:  "",
		},
		{label: "无任何文案", task: upstream.Task{}, want: ""},
	}
	for _, c := range cases {
		if got := expertNameFromTask(c.task); got != c.want {
			t.Errorf("%s: expertNameFromTask=%q want %q", c.label, got, c.want)
		}
	}
}

// TestResolveActionStaticFirst 静态表优先：Hp_Appearance 的标题也是「体验「X」」
// 形态，但它有登记动作，必须走静态表而不是被动态适配抢走。
func TestResolveActionStaticFirst(t *testing.T) {
	act := resolveAction(upstream.Task{TaskCode: "Hp_Appearance", Title: "体验「和平精英」主题"})
	if act == nil {
		t.Fatal("Hp_Appearance 应命中静态动作")
	}
	if act.TaskCode != "Hp_Appearance" || strings.Contains(act.Desc, "动态适配") {
		t.Errorf("应命中静态表而非动态适配：code=%q desc=%q", act.TaskCode, act.Desc)
	}
}

// TestResolveActionDynamicExpert 白名单外的专家类任务应能解析出动态动作。
func TestResolveActionDynamicExpert(t *testing.T) {
	act := resolveAction(upstream.Task{
		TaskCode: "expert_use_douyinyin",
		Title:    "体验「抖音策略师」",
		TaskDesc: "在专家页搜索并召唤「抖音策略师」，完成一次对话。",
	})
	if act == nil {
		t.Fatal("专家类新任务应动态适配出动作")
	}
	if !strings.Contains(act.Desc, "抖音策略师") {
		t.Errorf("动态动作 Desc 应含专家名：%q", act.Desc)
	}
	if act.run == nil {
		t.Error("动态动作必须带 run")
	}
}

// TestResolveActionNilForUnknown 识别不出的任务返回 nil（面板上显示为「待适配」，
// 不会进执行队列）——关注公众号任务要求真实关注满 24h，永不可自动化。
func TestResolveActionNilForUnknown(t *testing.T) {
	unknown := []upstream.Task{
		{TaskCode: "wb_wechat_oa_subscribe_task", Title: "关注「腾讯 WorkBuddy」官方公众号"},
		{TaskCode: "some_future_task", Title: "邀请好友注册"},
	}
	for _, tk := range unknown {
		if act := resolveAction(tk); act != nil {
			t.Errorf("%s 不应解析出动作，实际 %+v", tk.TaskCode, act)
		}
	}
}

// TestGrowthRunnableSplitsDisplayFromExecution 展示口径与执行口径必须分开：
// 识别不出动作的任务要能显示（growthPending=true）但不能执行（growthRunnable=false）。
func TestGrowthRunnableSplitsDisplayFromExecution(t *testing.T) {
	manual := upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task", Title: "关注「腾讯 WorkBuddy」官方公众号"}
	if !growthPending(manual) {
		t.Error("待适配任务必须进展示列表（否则上游上新看不见）")
	}
	if growthRunnable(manual) {
		t.Error("待适配任务不得进执行队列")
	}
	dyn := upstream.Task{TaskCode: "expert_use_x", TaskDesc: "在专家页搜索并召唤「某专家」，完成一次对话。"}
	if !growthRunnable(dyn) {
		t.Error("专家类新任务应可执行（动态适配）")
	}
	claimed := upstream.Task{TaskCode: "chat_5", Claimed: true}
	if growthPending(claimed) || growthRunnable(claimed) {
		t.Error("已领取任务两个口径都应为 false")
	}
}

// TestExpertIDFromTask 从 jump_url 取权威 expertId。
//
// 这个字段是「体验某专家」类任务能否真正计数的关键：任务文案里的名字不唯一
// （搜「抖音策略师」返回 4 个同名专家），只有 jump_url 里的 ID 是上游指定的目标。
// 用例取自 2026-10-30 线上真实 jump_url。
func TestExpertIDFromTask(t *testing.T) {
	cases := []struct {
		label string
		url   string
		want  string
	}{
		{
			label: "真实形态（外层 launch + urlencoded deeplink）",
			url:   "https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_FbY5nlWTqDod&buddy_type=workbuddy",
			want:  "ex_FbY5nlWTqDod",
		},
		{label: "已解码形态", url: "workbuddy://experts?expertId=ex_GDl31WahyIH0", want: "ex_GDl31WahyIH0"},
		{label: "带后续参数", url: "workbuddy://experts?expertId=ex_Weo3x2EZuSDm&from=task", want: "ex_Weo3x2EZuSDm"},
		{label: "无 expertId（Expert_lighthouse 形态）", url: "https://cloud.tencent.com/act/pro/workbuddy-lighthouse", want: ""},
		{label: "裸协议无查询串", url: "workbuddy://expert", want: ""},
		{label: "空", url: "", want: ""},
	}
	for _, c := range cases {
		got := expertIDFromTask(upstream.Task{JumpURL: c.url})
		if got != c.want {
			t.Errorf("%s: expertIDFromTask=%q want %q", c.label, got, c.want)
		}
	}
}

// TestMatchExpertsByName 同名专家必须全部返回，让调用方看出「不唯一」。
// 实测搜「抖音策略师」会返回 4 个命中——只取第一个会召唤到错的实例。
func TestMatchExpertsByName(t *testing.T) {
	experts := []upstream.MarketExpert{
		{ExpertID: "ex_A", DisplayNameZH: "短剧全域内容策略师", ProfessionZH: "短剧全域内容策略师"},
		{ExpertID: "ex_B", DisplayNameZH: "抖音策略师", ProfessionZH: "抖音短视频与直播电商策略专家"},
		{ExpertID: "ex_C", DisplayNameZH: "斗音音", ProfessionZH: "抖音策略师"},
		{ExpertID: "ex_D", DisplayNameZH: "抖音策略师", ProfessionZH: "抖音内容策略顾问"},
	}
	hits := matchExpertsByName(experts, "抖音策略师")
	if len(hits) != 3 {
		t.Fatalf("命中数=%d want 3（display 或 profession 任一等于即命中）", len(hits))
	}
	if len(matchExpertsByName(experts, "不存在的专家")) != 0 {
		t.Error("不存在的名字不应命中")
	}
}

// TestResolveActionCarriesExpertID 动态动作必须把 jump_url 里的 expertId 带进去——
// 不带就退化成按名匹配，会撞上同名专家。
func TestResolveActionCarriesExpertID(t *testing.T) {
	act := resolveAction(upstream.Task{
		TaskCode: "expert_use_douyinyin",
		TaskDesc: "在专家页搜索并召唤「抖音策略师」，完成一次对话。",
		JumpURL:  "https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_FbY5nlWTqDod&buddy_type=workbuddy",
	})
	if act == nil {
		t.Fatal("应动态适配出动作")
	}
	if !strings.Contains(act.Desc, "ex_FbY5nlWTqDod") {
		t.Errorf("Desc 应带权威 expertId：%q", act.Desc)
	}
}
