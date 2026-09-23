package panel

import "net/http"

// PromptSnapshot 当前生效的系统提示词，由 main 注入（读 Live 快照，保存后立即反映新值）。
type PromptSnapshot struct {
	Mode   string // passthrough / custom / append
	Text   string // 生效文本（custom/append 时非空）
	Source string // inline / file / builtin / none
}

// promptInfo 返回当前生效的系统提示词。
//
//	GET /panel/api/prompt
//
// 用途：面板「配置」页展示"网关此刻实际会注入什么"，并提供「载入当前生效」——
// 让用户在内置默认提示词的基础上改写，而不是从空白开始。
// 此前面板只有 prompt.file 一个路径输入框：看不到生效内容，也无法在面板里编辑提示词本体。
func (p *Panel) promptInfo(w http.ResponseWriter, r *http.Request) {
	if p.cfg.PromptInfo == nil {
		writeErr(w, http.StatusNotImplemented, "prompt info not available")
		return
	}
	s := p.cfg.PromptInfo()
	mode := s.Mode
	if mode == "" {
		mode = "passthrough"
	}
	source := s.Source
	if source == "" {
		source = "none"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"mode":   mode,
		"text":   s.Text,
		"source": source,
		// injected 报告是否真的会在出站前注入（custom/append 且文本非空）。
		// passthrough 或空文本时为 false——面板据此提示"当前不注入"。
		"injected": (mode == "custom" || mode == "append") && s.Text != "",
	})
}
