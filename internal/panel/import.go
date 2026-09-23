package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	TokenType     string `json:"token_type"`
	ExpiresAt     int64  `json:"expires_at"`
	Domain        string `json:"domain"`
	DosageNotify  string `json:"dosage_notify_code"`
	PaymentType   string `json:"payment_type"`
	Status        string `json:"status"`
	UsageUpdatedAt int64 `json:"usage_updated_at"`
	LastCheckin   int64  `json:"last_checkin_time"`
	CheckinStreak int    `json:"checkin_streak"`
	CreatedAt     int64  `json:"created_at"`
	LastUsed      int64  `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		expiresAt := acc.ExpiresAt / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if err := p.importAccount(a); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: %v", uid, err))
			continue
		}
		imported++
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// importAccount 把一个已归一化的账号落盘、热加载进池，并顺带签到/激活与余额刷新。
// 两条导入路径（cockpit 导出 / 本地部署 auths 目录）共用此函数。
//
// 返回 error 仅表示凭证落盘失败（账号未导入）；落盘成功后的附属动作失败只记日志——
// 账号此刻已可用，不该因签到失败而回滚。
//
// realm 判定统一走 ResolveRealm(存储标识, domain)：显式标识优先，否则按 domain 后缀推断。
// 这里不用 Auth.IsGlobal()，因为后者受 global 逃生门影响（关掉即恒判 cn），会把 global
// 账号的 realm 写死成 cn 永久污染凭证——逃生门只该锁路由，不该改写落盘数据。
func (p *Panel) importAccount(a *auth.Auth) error {
	uid := a.UID
	realm := auth.ResolveRealm(normalizeStoredRealm(a.RealmStored()), a.DomainValue())

	if realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			return fmt.Errorf("set realm: %w", err)
		}
	} else {
		_, _ = a.BackfillRealm()
	}

	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("save auth: %w", err)
	}

	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(uid) // 导入 = 人工恢复口径：清掉同 uid 旧条目遗留的禁用/冷却/熔断

	p.importSideEffects(a, realm)
	return nil
}

// normalizeStoredRealm 归一化 auth 文件里的 realm 标识：仅 cn/global 有效（大小写与空白
// 不敏感），其余（空/脏值）返回 "" 交给 domain 推断，避免脏值被 BackfillRealmFor 拒绝。
func normalizeStoredRealm(r string) string {
	switch v := strings.ToLower(strings.TrimSpace(r)); v {
	case "cn", "global":
		return v
	}
	return ""
}

// importSideEffects 导入成功后的尽力而为动作：global 走注册激活 + trial 领取，cn 走每日
// 签到；随后刷新余额并按积分解冻账号。全部幂等，失败只记日志不阻断导入。
// Upstream 未注入（测试 / 裁剪部署）时整体跳过。
func (p *Panel) importSideEffects(a *auth.Auth, realm string) {
	if p.cfg.Upstream == nil {
		return
	}
	uid := a.UID
	if realm == "global" {
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
		} else if activated {
			log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
		}
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			log.Printf("panel: import global trial uid=%s: %v", uid, err)
		} else if claimed {
			log.Printf("panel: import global trial uid=%s 已领", uid)
		}
	} else {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("panel: import checkin uid=%s: %v", uid, err)
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
	}
}
