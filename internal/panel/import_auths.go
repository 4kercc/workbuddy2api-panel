package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
)

// importAuths 导入本地部署（Windows / macOS / Linux 版）auths 目录下的凭证文件。
//
//	POST /panel/api/import/auths
//	Content-Type: multipart/form-data
//	Body: files=<file>[, files=<file>...]   （file 字段名亦兼容，便于 curl 单文件调用）
//
// 与 cockpit 导入的区别：cockpit 吃的是第三方工具导出的扁平数组，这里吃的是本项目
// **自身落盘的 auth 文件形态**——也就是把本地 exe 版的 auths/*.json 原样搬上服务器，
// 省去重新走一遍 OAuth。
//
// 前端允许直接选整个 auths 目录（webkitdirectory），因此目录里混入的 state.json /
// usage.json / config.json 属于常态：解析不出账号的文件静默计入 skipped，不报错。
func (p *Panel) importAuths(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	// files 为主字段（前端多选/目录选择）；file 兼容单文件调用。
	headers := append(r.MultipartForm.File["files"], r.MultipartForm.File["file"]...)
	if len(headers) == 0 {
		writeErr(w, http.StatusBadRequest, "missing files field")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, fh := range headers {
		f, err := fh.Open()
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: open failed: %v", fh.Filename, err))
			continue
		}
		// 单文件上限 8MiB：auth 文件实测 ~2.4KiB，给足余量同时挡掉误传大文件。
		raw, err := io.ReadAll(io.LimitReader(f, 8<<20))
		f.Close()
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: read failed: %v", fh.Filename, err))
			continue
		}

		accs, err := parseAuthBlob(raw)
		if err != nil {
			// 非凭证文件（state.json / usage.json 等）走到这里：静默跳过，不污染错误列表。
			skipped++
			continue
		}

		for _, a := range accs {
			total++
			// uid 来自文件内容且用于拼文件名，必须先过白名单（防 ../../ 路径穿越）。
			if !validImportUID(a.UID) {
				skipped++
				errs = append(errs, fmt.Sprintf("%s: invalid uid %q", fh.Filename, a.UID))
				continue
			}
			a.FilePath = filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", a.UID))
			if err := p.importAccount(a); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("%s: uid=%s: %v", fh.Filename, a.UID, err))
				continue
			}
			imported++
		}
	}

	log.Printf("panel: auths import finished files=%d accounts=%d imported=%d skipped=%d",
		len(headers), total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"files":    len(headers),
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// parseAuthBlob 从一个上传文件的内容里解析账号，自动识别两种形态：
//
//	单个 auth 对象：{"auth":{...},"account":{...}}（嵌套形）或 {"accessToken":...}（扁平形）
//	auth 对象数组：[{...},{...}]（数组里单个元素坏了不废掉整批）
//
// 解析不出任何账号时返回 error，调用方据此按"非凭证文件"静默跳过。
// 形态识别复用 auth.Parse，两条导入路径对文件格式的口径因此只有一份。
func parseAuthBlob(raw []byte) ([]*auth.Auth, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty file")
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("invalid json array: %w", err)
		}
		var out []*auth.Auth
		for _, it := range items {
			if a, err := auth.Parse(it); err == nil {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no valid account in array")
		}
		return out, nil
	}

	a, err := auth.Parse(trimmed)
	if err != nil {
		return nil, err
	}
	return []*auth.Auth{a}, nil
}
