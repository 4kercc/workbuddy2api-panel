// balance_401.go 余额探测 401 的「登录态求证」。
//
// 为什么单独成文件：余额刷新是池内唯一的**分钟级**健康观测量，但拿到错误此前只写一行
// 日志、不碰池状态——上游批量注销会话后，号在 billing/chat 两侧都已 401，面板仍显示
// 「可用」，要等每日 22:00 的保活连败满阈（≈3 天）才转 disabled，运维期间看不到任何信号
// （实测 3 个 CN 号连续 10 天余额 401，池仍报 15/15 可用）。
//
// 但**不能把 401 直接当 session dead 计罚**：401 有两种成因，
//  1. 会话被上游注销（refresh 回 12153）→ 需重登，应计罚；
//  2. 网关瞬时拦截（风控 / 边缘抖动 / 并发限流）→ 响应体与 1 完全一样（APISIX HTML 页），
//     号还能干活，计罚会误杀。
// 用 refresh 做一次权威求证即可分开（见 upstream.VerifySessionByRefresh）。
// 非 401 不参与：5xx / 限流 / 传输层各有既有惩罚路径。
package scheduler

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
	"github.com/4kercc/workbuddy2api-panel/internal/logfmt"
	"github.com/4kercc/workbuddy2api-panel/internal/pool"
	"github.com/4kercc/workbuddy2api-panel/internal/upstream"
)

// authHealBackoff 同一账号 401→refresh 求证的最小间隔。降级 token 每轮探测都会 401，
// 若每轮都刷新，一天会轮换 288 次 RT。
const authHealBackoff = 30 * time.Minute

// noteBalanceAuthFailure 余额探测拿到错误时的处置：只对 401 做求证，其余原样返回。
func (s *Scheduler) noteBalanceAuthFailure(a *auth.Auth, uid string, probeErr error) {
	var ue *upstream.Error
	if !errors.As(probeErr, &ue) || ue.Status != http.StatusUnauthorized {
		return
	}
	// 没有 RT 无从求证（静态 key / 企业号等形态）：保持「只记日志」的旧语义。
	if a.RefreshTokenValue() == "" {
		return
	}
	label := logfmt.Label(uid, a.Nickname)
	// 退避只记「refresh 成功」：真死号每轮 refresh 都回 12153 且不轮换 RT，必须每轮都
	// 试，连续计数才能按预期在阈值内判死；把失败也记进退避会让死号永远停在 1/阈值。
	if !s.allowAuthHeal(uid) {
		log.Printf("balance %s: 401（%s 内已求证过登录态，本轮跳过 refresh）", label, authHealBackoff)
		return
	}
	rerr := s.cfg.Upstream.RefreshToken(a)
	if rerr == nil {
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("balance %s: 401 后刷新成功但落盘失败: %v", label, serr)
		}
		s.noteAuthHeal(uid)
		s.cfg.Pool.ClearSessionDead(uid)
		log.Printf("balance %s: 401 经 refresh 求证为降级 token（会话仍活），已换新 token 并清 session dead 计数", label)
		return
	}
	var re *upstream.Error
	// Kind 之外再看一次 code=12153：doJSON 只用 env.Msg 做分类，而上游的 12153 文案
	// 不总是英文原文（本地化后不含 marker 就会落成 ErrClient，死号便永远不计罚）。
	if errors.As(rerr, &re) && (re.Kind == upstream.ErrSessionDead || strings.Contains(re.Msg, "12153")) {
		// refresh token 失效 ≠ 账号不可用：AT 未过期时 chat 仍正常（与保活同一判据）。
		// 计死会在连续 3 轮后禁用掉一个健康账号。
		if upstream.SessionDeadButUsable(a) {
			log.Printf("balance %s: refresh token 已失效但 access token 仍有效，不计 session dead", label)
			return
		}
		if s.cfg.Pool.NoteSessionDeadReason(uid, fmt.Sprintf(
			"网关 401 × %d 且 refresh 验证失败（token 已确认失效）", pool.SessionDeadThreshold())) {
			log.Printf("balance %s: 连续 %d 次 401 且 refresh 判死 — 禁用（需重新登录）", label, pool.SessionDeadThreshold())
			return
		}
		if st, ok := s.cfg.Pool.Status(uid); ok {
			log.Printf("balance %s: 401 且 refresh 判死（session dead 计数 %d/%d）", label, st.SessionDeadFails, pool.SessionDeadThreshold())
		}
		return
	}
	log.Printf("balance %s: 401 但 refresh 失败（非 session dead，不计罚）: %v", label, rerr)
}

// allowAuthHeal 退避窗口判定。
func (s *Scheduler) allowAuthHeal(uid string) bool {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if t, ok := s.authHealAt[uid]; ok && time.Since(t) < authHealBackoff {
		return false
	}
	return true
}

// noteAuthHeal 记录一次「refresh 成功」的求证时刻。
func (s *Scheduler) noteAuthHeal(uid string) {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if s.authHealAt == nil {
		s.authHealAt = make(map[string]time.Time)
	}
	s.authHealAt[uid] = time.Now()
}
