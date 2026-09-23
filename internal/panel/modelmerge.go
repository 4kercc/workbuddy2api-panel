package panel

import "strings"

// 模型汇聚：「模型与档位」视图把两域同名模型合并成一条显示。
//
// 为什么只作用于面板展示、**不作用于 /v1/models**：model 名的 realm 前缀是网关侧
// 路由信号（internal/server.resolveModel），而无前缀默认判 cn —— 若把 /v1/models
// 也汇聚成裸名，global 独有模型会被静默路由到 CN 账号，去打上游不存在的模型。
//
// 为什么汇聚时保留每域完整条目而非取其一：实测 9 个同名模型中 7 个两域元数据不同，
// 且差异有实际影响——global 的 glm-5.3-flash 最大输出 32000（CN 为 131072）；
// deepseek-v4.1-flash 积分倍率 CN x0.03 / global x0.00、思考档位 CN 三档 / global
// 仅 high；hy4-preview-f 的视觉与工具调用能力仅 CN 支持。取其一会让用户按错误的上限
// 与成本预期使用模型，故两值都保留，由前端逐列对比呈现。

// splitRealmID 拆 "cn:xxx" / "global:xxx" → (realm, bare)；无前缀或未知前缀 → ("", 原值)。
func splitRealmID(id string) (realm, bare string) {
	if i := strings.IndexByte(id, ':'); i > 0 {
		switch id[:i] {
		case "cn", "global":
			return id[:i], id[i+1:]
		}
	}
	return "", id
}

// mergeModelEntries 把逐域模型条目按裸模型名汇聚：
//
//	跨域同名 → 合成一条 {id: 裸名, merged: true, realms: [...], variants: [各域原条目]}
//	仅单域   → 原样保留（id 仍带前缀，前端走普通分支，无需特判）
//
// 输出顺序稳定：跨域项落在其首次出现的位置，单域项原地保留。开关来回切换时行序不变，
// 便于前后对比。输入为空时返回空切片（非 nil，保证 JSON 序列化为 [] 而非 null）。
func mergeModelEntries(entries []map[string]any) []map[string]any {
	order := make([]string, 0, len(entries))
	byBare := make(map[string][]map[string]any, len(entries))
	for _, e := range entries {
		id, _ := e["id"].(string)
		_, bare := splitRealmID(id)
		if _, seen := byBare[bare]; !seen {
			order = append(order, bare)
		}
		byBare[bare] = append(byBare[bare], e)
	}

	out := make([]map[string]any, 0, len(order))
	for _, bare := range order {
		variants := byBare[bare]
		if len(variants) == 1 {
			out = append(out, variants[0])
			continue
		}
		realms := make([]string, 0, len(variants))
		for _, v := range variants {
			if r, _ := splitRealmID(v["id"].(string)); r != "" {
				realms = append(realms, r)
			}
		}
		out = append(out, map[string]any{
			"id":       bare,
			"merged":   true,
			"realms":   realms,
			"variants": variants,
		})
	}
	return out
}
