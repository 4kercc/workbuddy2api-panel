// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string        // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏

	// 系统提示词（custom/append 模式在出站前注入）。此前是 handler 的静态字段，
	// 面板改了不生效也不提示重启（静默失效），故收编进快照以支持热生效。
	// PromptMode 空 = 快照未携带该组字段，调用方回退静态值（测试/裸用场景）。
	PromptMode   string
	PromptText   string
	PromptSource string // inline / file / builtin / none，仅用于面板展示来源

	// PanelModelMerge 面板「模型与档位」视图汇聚两域同名模型（纯展示开关，
	// 不影响 /v1/models 与选号路由）。面板保存配置后立即生效。
	PanelModelMerge bool

	// CrossRealmModels 跨域模型白名单（routing.cross_realm_models）：**裸模型名**命中时
	// 该请求不做选号域过滤，CN 与 global 账号同为候选 —— 让同一个模型名在两域账号间通用。
	// 显式 "cn:" / "global:" 前缀仍然锁域（显式意图不被配置覆盖）。
	// 元素应为裸名（不带前缀）；切片在快照里按只读使用，Store 时整体替换。
	CrossRealmModels []string
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }
