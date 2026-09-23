package server

import "strings"

// 模型名 → 选号域 的解析（PLAN D6 + 跨域白名单）。
//
// 分两层：
//
//	resolveModelEx —— 纯前缀协议解析，报告 realm 是显式前缀还是缺省推断；
//	ResolveRoute   —— 对外唯一入口：在前缀协议上叠加"跨域白名单"，得到最终选号域。
//
// 两处调用方（chat 主链路 handler.go、会话粘性可用集 wiring.go 的闭包）**必须**
// 走 ResolveRoute：粘性分配与实际选号若用不同口径，跨域请求会被粘到与该口径不符的号上。

// resolveModelEx 解析模型名协议：
//
//	带前缀 "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离，explicit=true；否则视为裸名，
// realm=cn（缺省）、bare=原串、explicit=false。大小写敏感（前缀须为精确小写枚举）。
// bare 即出站/选号/账本使用的裸模型名。
func resolveModelEx(model string) (realm, bare string, explicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model, false
	}
	return prefix, model[idx+1:], true
}

// ResolveRoute 解析模型名得到最终选号域 (realm, bareModel)。
//
// 规则（优先级从高到低）：
//
//	1. 显式前缀（"cn:" / "global:"）→ 强制该域，**跨域白名单不覆盖**（显式意图不可被配置推翻）；
//	2. 裸名且 bare 命中 crossRealm 白名单 → realm 返回 ""，池内语义为**不做域过滤**，
//	   两域账号同为候选（跨域使用同一模型名）；
//	3. 其余裸名 → "cn"（现状零回归）。
//
// crossRealm 为空（缺省配置）时行为与仅做前缀解析完全一致，老部署零回归。
//
// 注意 realm=="" 只在池的选号谓词里表示"不过滤"；出站模型名始终用 bare（前缀是网关侧
// 路由协议，上游只认裸名），故跨域不会把前缀泄漏给上游。
func ResolveRoute(model string, crossRealm []string) (realm, bare string) {
	realm, bare, explicit := resolveModelEx(model)
	if explicit {
		return realm, bare
	}
	for _, m := range crossRealm {
		if m == bare {
			return "", bare
		}
	}
	return realm, bare
}
