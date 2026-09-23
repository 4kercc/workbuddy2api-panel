package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。
//
// 走 server.ResolveRoute（而非直接解析前缀）是刻意的：粘性可用集与实际 chat 选号必须
// **同一口径**。跨域白名单（routing.cross_realm_models）下裸名对应两域全集，若这里漏了
// 这一步，跨域请求会被粘到与选号口径不符的号上（粘性命中即跳过选号，白名单等于失效）。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为 AvailableUIDsForModel（不过滤）。
func realmAwareAvailableForModel(p *pool.Pool, live *livecfg.Holder) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveRoute(model, live.Load().CrossRealmModels)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}
