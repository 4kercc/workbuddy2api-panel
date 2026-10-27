// chat.go 面板「对话测试」接口：用指定账号真实发一次 chat 请求（直连上游、不走池
// 轮转），把上游的真实结果回给运维。
//
// 用途（本接口的核心价值）：**分辨「真封禁」与「假封禁」**。
// 面板里账号被标「已禁用」的原因可能有两类——
//   - 真封禁：上游判 session/鉴权失效（401 / 12153 / 403 auth），凭证已死，必须重登；
//   - 假封禁：本地误判（如 2026-10 的 11128 "request illegal" 反探测被误归账号封禁），
//     账号其实完全健康——签到/余额/对话都正常，只是被本地状态机禁用了。
//
// 签到与余额走 billing 域，判据较弱（billing 通了不代表 chat 可用）。本接口走 chat 域
// （与 /v1 同链路、同头族），判据最强：能出模型回复 = 账号可对外服务。
// 因此对「已禁用」账号做一次对话测试即可判定：出回复 = 假封禁（点解冻即可恢复）；
// 返回 401/12153 = 真封禁（需重走 OAuth）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/4kercc/workbuddy2api-panel/internal/upstream"
)

// chatProbeDefaultModel 对话测试默认模型：全账号（CN/global）可用性最广的轻量模型。
// 前端可传 model 覆盖（面板按钮默认用此值）。
const chatProbeDefaultModel = "deepseek-v4.1-flash"

// chatProbeDefaultPrompt 对话测试默认提示词：极短，消耗可忽略（≈10 token）。
const chatProbeDefaultPrompt = "你好，请用一句话确认你已收到本条测试消息。"

// chatProbeMaxStream 对话测试读流上限（4MB）：异常/超大流不至于把面板进程读爆。
const chatProbeMaxStream = 4 << 20

// accountChat 单号对话测试。请求体（可选）：{"model":"...","prompt":"..."}。
//
// 响应恒为 HTTP 200（探活语义：失败信息装在 body 里，前端据 ok 字段区分）：
//
//	{ok:true,  status:200, model, elapsed_ms, reply:"模型回复"}
//	{ok:false, status:400, model, elapsed_ms, error:"上游原文", kind:"content_blocked"}
func (p *Panel) accountChat(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var in struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if r.Body != nil {
		// 1MB 上限防畸形/超大 body；解析失败按全默认（不报错——测试请求体可选）。
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = chatProbeDefaultModel
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = chatProbeDefaultPrompt
	}

	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"stream":   true,
	})

	started := time.Now()
	rc, status, respBody, err := p.cfg.Upstream.ChatStream(a, body, "", upstream.ChatMeta{})
	elapsed := time.Since(started).Milliseconds()
	if rc != nil {
		defer rc.Close()
	}

	resp := map[string]any{
		"ok":         false,
		"uid":        uid,
		"model":      model,
		"elapsed_ms": elapsed,
		"status":     status,
	}

	// 失败路径：上游 ≥400 或传输层错误。透传上游原文 + 分类 kind（前端直接显示
	// 「内容拦截 / 账号封禁 / 限流 …」，无需运维自己认错误码）。
	if err != nil || status >= 400 {
		raw := strings.TrimSpace(string(respBody))
		if raw == "" && err != nil {
			raw = err.Error()
		}
		resp["error"] = raw
		if status >= 400 {
			resp["kind"] = upstream.Classify(status, string(respBody)).String()
		}
		log.Printf("panel: chat-test uid=%s model=%s status=%d elapsed=%dms err=%.160s",
			uid, model, status, elapsed, raw)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 成功路径：聚合 SSE 为单个 OpenAI 响应，取 choices[0].message.content。
	agg, aerr := upstream.Aggregate(io.LimitReader(rc, chatProbeMaxStream))
	if aerr != nil {
		resp["error"] = "aggregate stream: " + aerr.Error()
		log.Printf("panel: chat-test uid=%s model=%s status=%d aggregate failed: %v", uid, model, status, aerr)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["ok"] = true
	resp["reply"] = extractChatReply(agg)
	log.Printf("panel: chat-test uid=%s model=%s status=200 elapsed=%dms reply=%.80s",
		uid, model, elapsed, resp["reply"])
	writeJSON(w, http.StatusOK, resp)
}

// extractChatReply 从聚合后的 OpenAI 响应里取回复正文（choices[0].message.content）。
// 取不到返回空串（前端显示「（空回复）」）——不编造内容。
func extractChatReply(agg map[string]any) string {
	choices, ok := agg["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		return ""
	}
	content, _ := msg["content"].(string)
	return strings.TrimSpace(content)
}
