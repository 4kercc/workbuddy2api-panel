// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 开学季独立状态。解决"不知道哪些账号有哪些任务没做"与"开学季状态不可见"。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表 + 开学季任务列表，
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
//   - 开学季（school/status + school/run_all）：独立状态视图 + 一键闭环。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
	"github.com/4kercc/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// schoolTaskView 开学季任务条目（面板展示口径）。
type schoolTaskView struct {
	Code   string `json:"task_code"`
	Status string `json:"status"` // pending | in_progress | completed | claimed
	Prog   int    `json:"progress"`
	Target int    `json:"target_count"`
}

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string           `json:"uid"`
	Nickname  string           `json:"nickname"`
	Growth    []upstream.Task  `json:"growth,omitempty"`
	GrowthErr string           `json:"growth_error,omitempty"`
	School    []schoolTaskView `json:"school,omitempty"`
	SchoolErr string           `json:"school_error,omitempty"`
	InPeriod  bool             `json:"in_period"`
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		return false // 达标未领：也入队（队列执行后会自动领）
	}
	return autoActionFor(t.TaskCode) != nil
}

// schoolPending 开学季任务是否待办（排除学生认证）。
func schoolPending(t upstream.SchoolTask) bool {
	switch t.TaskCode {
	case "task_student_verify":
		return false // 需微信学生真实认证
	case "desktop_chat_1_time":
		return t.Status != "claimed"
	default:
		return t.Status != "claimed" && t.Status != "completed"
	}
}

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化）+ 开学季（未完成）。
// 只读操作，并发拉取（账号数个位数）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.Nickname
			// D4 门控：global 账号无 CN 成长/开学季任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
			} else {
				for _, t := range tasks {
					if growthPending(t) {
						it.Growth = append(it.Growth, t)
					}
				}
			}
			// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
			// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
			// 静默（无 mp 任务的部署/活动结束时零影响）。
			if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
				seen := map[string]bool{}
				for _, t := range it.Growth {
					seen[t.TaskCode] = true
				}
				for _, t := range mpTasks {
					if growthPending(t) && !seen[t.TaskCode] {
						it.Growth = append(it.Growth, t)
					}
				}
			}
			if stasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a); err != nil {
				it.SchoolErr = err.Error()
			} else {
				it.InPeriod = inPeriod
				for _, t := range stasks {
					if schoolPending(t) {
						it.School = append(it.School, schoolTaskView{
							Code: t.TaskCode, Status: t.Status, Prog: t.Progress, Target: t.TargetCount,
						})
					}
				}
			}
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth) + len(it.School)
	}
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项（成长+开学季）", pending)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth | school
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	starting  bool // 已抢占启动权（正在扫描/组装）：并发启动的第二轮被拒
	startedAt time.Time
	endedAt   time.Time
	items     []queueItem
	conc      int
	seq       int
	lastMsg   string // 上一轮结果摘要（面板状态与日志共用）
	autoRuns  int    // 自动执行已跑轮次
}

// begin 抢占启动权（扫描 + 执行期间独占）；已有轮次在跑或正在启动返回 false。
//
// 手动「执行全部待办」与自动执行共用这道闸：两者并发会让同一账号的任务动作
// 跑两遍（动作幂等，但 expert 系每遍含 8 次真实对话，白烧配额）。
// starting 覆盖"扫描中"这段窗口——只在扫描后才置 running，否则并发启动会漏。
func (q *queueState) begin() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running || q.starting {
		return false
	}
	q.starting = true
	return true
}

// commit 把组装好的条目转为运行中并返回本轮代次；items 为空时释放启动权、
// 返回 (0,false)（调用方按"无待办"处理）。
func (q *queueState) commit(items []queueItem, conc int) (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.starting = false
	if len(items) == 0 {
		return 0, false
	}
	q.running = true
	q.startedAt = time.Now()
	q.items = items
	q.conc = conc
	q.seq++
	return q.seq, true
}

// abort 释放启动权（扫描阶段异常/放弃）。
func (q *queueState) abort() {
	q.mu.Lock()
	q.starting = false
	q.mu.Unlock()
}

// finish 标记本轮结束并记录结果摘要。
func (q *queueState) finish(msg string) {
	q.mu.Lock()
	q.running = false
	q.endedAt = time.Now()
	q.lastMsg = msg
	q.mu.Unlock()
}

// summary 统计条目状态（done/failed/skipped/pending）。
func (q *queueState) summary() (done, failed, skipped, pending int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.items {
		switch it.Status {
		case "done":
			done++
		case "error":
			failed++
		case "skipped":
			skipped++
		default:
			pending++
		}
	}
	return
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// queuePlan 一轮队列的组装结果：账号单元（执行分组）+ 条目（展示/状态）。
type queuePlan struct {
	accts []queueAccount
	items []queueItem
}

// planQueue 扫描全部账号待办并组装队列（只读，不启动执行）。
// 手动「执行全部待办」与自动执行共用这一份口径——两处各写一份曾导致
// "扫描显示 mp 待办而队列报无可执行待办"。
func (p *Panel) planQueue(growth, school bool) queuePlan {
	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, wantSchool bool) {
			defer wg.Done()
			one := queueAccount{a: a}
			// D4 门控：global 账号无 CN 成长/开学季任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			if growth {
				if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
					for _, t := range tasks {
						if growthPending(t) {
							one.grow = append(one.grow, t)
						}
					}
					// 合并小程序口径待办（与 tasksScanAll 同口径：mp 列表是默认口径
					// 超集，按 code 去重；失败静默）。此前此处漏合并——扫描显示
					// mp 待办而队列报"无可执行待办"。
					if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
						seen := map[string]bool{}
						for _, t := range one.grow {
							seen[t.TaskCode] = true
						}
						for _, t := range mpTasks {
							if growthPending(t) && !seen[t.TaskCode] {
								one.grow = append(one.grow, t)
							}
						}
					}
					sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
						return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if wantSchool && p.cfg.Scheduler != nil {
				if stasks, _, err := p.cfg.Upstream.SchoolTasks(a); err == nil {
					for _, t := range stasks {
						if schoolPending(t) { // 认证等不可做任务已在口径外
							one.school = true
							break
						}
					}
				}
			}
			if len(one.grow) > 0 || one.school {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a, school)
	}
	wg.Wait()

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
		if one.school {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "school", Code: "school_daily", Status: "pending"})
		}
	}
	return queuePlan{accts: accts, items: items}
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
		School      bool `json:"school"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth && !body.School {
		body.Growth, body.School = true, true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	q := p.queue()
	if !q.begin() {
		writeErr(w, http.StatusConflict, "队列正在执行中（可在任务中心查看进度）")
		return
	}

	plan := p.planQueue(body.Growth, body.School)
	seq, started := q.commit(plan.items, body.Concurrency)
	if !started {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": "全部账号没有待办任务"})
		return
	}

	go p.runQueueItems(plan.accts, plan.items, body.Concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v 开学季 %v）", len(plan.items), body.Concurrency, body.Growth, body.School)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": len(plan.items), "seq": seq})
}

// ---------------------------------------------------------------------------
// 自动扫描 + 自动执行
// ---------------------------------------------------------------------------

// SetTaskAuto 同步「任务队列自动扫描+执行」的展示快照（main 启动装配与保存配置后调用）。
// hours/conc 非法值就地归一，保证状态接口永远返回可展示的值。
func (p *Panel) SetTaskAuto(enabled bool, hours []int, conc int) {
	if conc < 1 {
		conc = 1
	}
	if conc > 4 {
		conc = 4
	}
	hs := make([]int, len(hours))
	copy(hs, hours)
	p.autoMu.Lock()
	p.autoOn, p.autoHours, p.autoConc = enabled, hs, conc
	p.autoMu.Unlock()
}

// autoSnapshot 读展示快照（开关/时点/并发）。
func (p *Panel) autoSnapshot() (bool, []int, int) {
	p.autoMu.Lock()
	defer p.autoMu.Unlock()
	hs := make([]int, len(p.autoHours))
	copy(hs, p.autoHours)
	return p.autoOn, hs, p.autoConc
}

// autoConcurrency 自动执行的账号并发（未配置回落 1：默认单号串行，最保守）。
func (p *Panel) autoConcurrency() int {
	_, _, conc := p.autoSnapshot()
	if conc < 1 {
		return 1
	}
	return conc
}

// noteAutoRun 记一次自动执行轮次。
func (q *queueState) noteAutoRun() {
	q.mu.Lock()
	q.autoRuns++
	q.mu.Unlock()
}

// RunAutoQueueNow 执行一轮自动任务队列：扫描全部账号待办 → 有待办则排队执行 →
// 等待跑完并记录结果。由 scheduler 的 task_auto_hours 时点触发（见 cmd/server
// 注入的 scheduler.Config.TaskAutoFn），也可由面板手动触发一轮。
//
// 同步阻塞直到本轮结束——调用方（scheduler）只做到点触发，注入的回调里已 go 出去，
// 排程主循环不被长任务拖住。
// 并发语义：与手动「执行全部待办」共用 queueState 启动闸，队列已在跑时本轮直接
// 跳过（不排队等待——下个时点会再来一轮，堆积反而放大上游压力）。
func (p *Panel) RunAutoQueueNow() {
	q := p.queue()
	if !q.begin() {
		log.Printf("panel: 自动任务队列跳过：队列正在执行中")
		return
	}
	plan := p.planQueue(true, true)
	conc := p.autoConcurrency()
	_, started := q.commit(plan.items, conc)
	if !started {
		q.finish("无待办任务")
		log.Printf("panel: 自动任务队列：全部账号无待办（未发起任何任务动作）")
		return
	}
	q.noteAutoRun()
	log.Printf("panel: 自动任务队列启动：%d 项（并发 %d）", len(plan.items), conc)
	p.runQueueItems(plan.accts, plan.items, conc)

	done, failed, skipped, pending := q.summary()
	log.Printf("panel: 自动任务队列结束：成功 %d / 失败 %d / 跳过 %d / 未处理 %d", done, failed, skipped, pending)
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		done, failed, skipped, pending := q.summary()
		q.finish(fmt.Sprintf("成功 %d / 失败 %d / 跳过 %d / 未处理 %d", done, failed, skipped, pending))
		log.Printf("panel: 队列执行结束（共 %d 项：成功 %d 失败 %d 跳过 %d 未处理 %d）", len(items), done, failed, skipped, pending)
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				case "school":
					msg, err = p.runSchoolQueued(one.a)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a      *auth.Auth
	grow   []upstream.Task
	school bool
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// runSchoolQueued 单账号开学季闭环（scheduler 四任务 + 抽奖）。
func (p *Panel) runSchoolQueued(a *auth.Auth) (string, error) {
	if p.cfg.Scheduler == nil {
		return "", fmt.Errorf("scheduler 不可用")
	}
	p.cfg.Scheduler.RunSchoolAccountNow(a)
	// 闭环后回读开学季状态做汇总。
	tasks, _, err := p.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return "闭环已执行（状态回读失败）", nil
	}
	done := 0
	for _, t := range tasks {
		if t.Status == "claimed" || (t.TaskCode != "task_student_verify" && t.Progress >= t.TargetCount && t.TargetCount > 0) {
			done++
		}
	}
	log.Printf("panel: 队列 school uid=%s: 闭环完成（%d/%d 项完成）", a.UID, done, len(tasks))
	return fmt.Sprintf("开学季闭环完成（%d/%d 项已完成，抽奖已抽完）", done, len(tasks)), nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	running, startedAt, endedAt, conc, seq, lastMsg := q.running, q.startedAt, q.endedAt, q.conc, q.seq, q.lastMsg
	q.mu.Unlock()
	out := map[string]any{
		"running":    running,
		"total":      len(items),
		"conc":       conc,
		"started":    !startedAt.IsZero(),
		"started_at": startedAt,
		"seq":        seq,
		"items":      items,
	}
	if !endedAt.IsZero() {
		out["ended_at"] = endedAt
		out["last_result"] = lastMsg
	}
	writeJSON(w, http.StatusOK, out)
}

// tasksAutoStatus 「自动扫描 + 自动执行」状态：开关/时点/并发 + 下次执行时刻 +
// 上一轮结果。排程真值在 scheduler（快照经 SetTaskAuto 同步），下次时刻由
// scheduler.NextTaskAutoAt 给出——与排程主循环同一份 nextFire 口径，不重复实现。
func (p *Panel) tasksAutoStatus(w http.ResponseWriter, r *http.Request) {
	on, hours, conc := p.autoSnapshot()
	q := p.queue()
	q.mu.Lock()
	running, lastMsg, lastEnd, runs := q.running, q.lastMsg, q.endedAt, q.autoRuns
	q.mu.Unlock()

	out := map[string]any{
		"ok":          true,
		"enabled":     on,
		"hours":       hours,
		"concurrency": conc,
		"running":     running,
		"auto_runs":   runs,
	}
	if !lastEnd.IsZero() {
		out["last_run"] = lastEnd.Format(time.RFC3339)
		out["last_result"] = lastMsg
	}
	if on && p.cfg.Scheduler != nil {
		if next := p.cfg.Scheduler.NextTaskAutoAt(time.Now()); !next.IsZero() {
			out["next_at"] = next.Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 开学季独立视图
// ---------------------------------------------------------------------------

// schoolStatus 全账号开学季任务状态（含抽奖余额）。
func (p *Panel) schoolStatus(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	type acctView struct {
		UID      string           `json:"uid"`
		Nickname string           `json:"nickname"`
		InPeriod bool             `json:"in_period"`
		Tasks    []schoolTaskView `json:"tasks"`
		Chances  int              `json:"chances"`
		Err      string           `json:"error,omitempty"`
	}
	out := make([]acctView, 0, len(states))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			v := acctView{UID: a.UID, Nickname: a.Nickname}
			// D4 门控：global 账号无开学季活动，不发起任何上游调用。
			if a.IsGlobal() {
				v.Err = "global realm（无开学季活动）"
				mu.Lock()
				out = append(out, v)
				mu.Unlock()
				return
			}
			tasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a)
			if err != nil {
				v.Err = err.Error()
			} else {
				v.InPeriod = inPeriod
				for _, t := range tasks {
					v.Tasks = append(v.Tasks, schoolTaskView{
						Code: t.TaskCode, Status: t.Status, Prog: t.Progress, Target: t.TargetCount,
					})
				}
			}
			v.Chances, _ = p.cfg.Upstream.SchoolChances(a)
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Nickname < out[j].Nickname })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// schoolRunAll 一键执行全部账号开学季闭环（异步，进度看任务频道日志）。
func (p *Panel) schoolRunAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunSchoolNow()
	log.Printf("panel: 开学季全账号闭环已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
