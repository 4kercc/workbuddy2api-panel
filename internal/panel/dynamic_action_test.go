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
