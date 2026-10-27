// export.go 面板「导出配置」接口：把账号凭证与网关配置打包成单个 JSON 文件下载，
// 便于迁移到另一实例（换服务器 / 重建部署）时免去重新走 OAuth。
//
// 与 import_auths.go 对称：导出的 accounts 数组元素是**嵌套形** auth 文档（与
// auths/workbuddy-<uid>.json 落盘格式逐字一致），因此该文件可直接经
// 「添加账号 → 本地部署导入」重新导入（parseAuthBlob 已识别 {accounts:[...]} 备份包）。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// exportBundle 备份包结构。accounts 为嵌套形 auth 文档数组；config 为 config.json
// 内容（LoadConfig 未注入时为 null）。
type exportBundle struct {
	App        string           `json:"app"`
	Version    string           `json:"version"`
	ExportedAt string           `json:"exported_at"`
	Accounts   []map[string]any `json:"accounts"`
	Config     any              `json:"config"`
}

// exportConfig 导出账号凭证 + 配置为单个 JSON 下载。
//
//	GET /panel/api/export
//
// 响应带 Content-Disposition: attachment，浏览器直接落盘为 wb2api-backup-<时间>.json。
// 安全提示：文件含 accessToken / refreshToken（等同账号密码），只应保存在可信位置。
func (p *Panel) exportConfig(w http.ResponseWriter, r *http.Request) {
	accounts, skipped := p.readAuthDocs()
	bundle := exportBundle{
		App:        "workbuddy2api-panel",
		Version:    p.cfg.Version,
		ExportedAt: time.Now().Format(time.RFC3339),
		Accounts:   accounts,
	}
	if p.cfg.LoadConfig != nil {
		if cfg, err := p.cfg.LoadConfig(); err == nil {
			bundle.Config = cfg
		} else {
			log.Printf("panel: export load config failed: %v", err)
		}
	}
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal bundle: "+err.Error())
		return
	}
	fname := "wb2api-backup-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	log.Printf("panel: export bundle accounts=%d skipped=%d bytes=%d", len(accounts), skipped, len(raw))
}

// readAuthDocs 读取 AuthDir 下全部 workbuddy-*.json 凭证文件为嵌套形文档。
//
// 直接读盘而非读内存：① 盘上文件是 SaveAtomic 的原子写一致快照，避免与 RefreshToken
// 并发改写 token 字段的撕裂读（AccessToken/RefreshToken/ExpiresAt 三个字段无法在一次
// 加锁内一并取出——sync.Mutex 不可重入，逐个 accessor 取值可能跨一次刷新，出现
// 「新 accessToken + 旧 refreshToken」的坏快照）；② 逐字就是导入侧期望的格式，往返无损。
// 单个文件读/解析失败计入 skipped，不阻断整体导出。
func (p *Panel) readAuthDocs() (docs []map[string]any, skipped int) {
	dir := strings.TrimSpace(p.cfg.AuthDir)
	if dir == "" {
		return nil, 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("panel: export read auths dir failed: %v", err)
		return nil, 0
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "workbuddy-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names) // 稳定输出顺序，便于人工 diff / 版本对比
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			skipped++
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			skipped++
			continue
		}
		docs = append(docs, doc)
	}
	return docs, skipped
}
