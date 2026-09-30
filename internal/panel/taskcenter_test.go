package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/4kercc/workbuddy2api-panel/internal/auth"
	"github.com/4kercc/workbuddy2api-panel/internal/pool"
	"github.com/4kercc/workbuddy2api-panel/internal/scheduler"
	"github.com/4kercc/workbuddy2api-panel/internal/upstream"
)

// TestQueueGateExclusive 队列启动闸：一轮在跑时第二轮必须被拒。
// 手动「执行全部待办」与自动执行共用这道闸——两者并发会让同一账号的任务动作
// 跑两遍（动作幂等，但 expert 系每遍含 8 次真实对话，白烧配额）。
func TestQueueGateExclusive(t *testing.T) {
	q := &queueState{}
	if !q.begin() {
		t.Fatal("首轮应能抢占启动权")
	}
	if q.begin() {
		t.Fatal("扫描中第二轮应被拒（starting 窗口）")
	}
	seq, started := q.commit([]queueItem{{UID: "u1", Kind: "growth", Code: "chat_5", Status: "pending"}}, 1)
	if !started || seq != 1 {
		t.Fatalf("commit: seq=%d started=%v want 1/true", seq, started)
	}
	if q.begin() {
		t.Fatal("运行中第二轮应被拒")
	}
	q.mu.Lock()
	q.items[0].Status = "done"
	q.mu.Unlock()
	done, failed, skipped, pending := q.summary()
	if done != 1 || failed != 0 || skipped != 0 || pending != 0 {
		t.Errorf("summary=%d/%d/%d/%d want 1/0/0/0", done, failed, skipped, pending)
	}
	q.finish("成功 1 / 失败 0 / 跳过 0 / 未处理 0")
	if !q.begin() {
		t.Fatal("上一轮结束后应可再启动")
	}
	q.abort()
	if !q.begin() {
		t.Fatal("abort 后启动权应已释放")
	}
}

// TestQueueCommitEmptyReleasesGate 空队列（全部账号无待办）不进入运行态，
// 且立刻释放启动权——否则一次"无待办"的自动轮次会把后续所有轮次永久堵死。
func TestQueueCommitEmptyReleasesGate(t *testing.T) {
	q := &queueState{}
	if !q.begin() {
		t.Fatal("begin 应成功")
	}
	if _, started := q.commit(nil, 1); started {
		t.Fatal("空队列不应启动")
	}
	if !q.begin() {
		t.Fatal("空队列后应立刻可再启动（启动权已释放）")
	}
	q.abort()
}

// TestSetTaskAutoSnapshot 自动执行展示快照：并发越界归一，切片按值拷贝
// （调用方后续改动自己的切片不得影响面板状态）。
func TestSetTaskAutoSnapshot(t *testing.T) {
	p := &Panel{}
	hours := []int{10, 20}
	p.SetTaskAuto(true, hours, 9) // 9 越界 → 归一为 4
	hours[0] = 3                  // 外部改动不得影响快照

	on, got, conc := p.autoSnapshot()
	if !on {
		t.Error("enabled want true")
	}
	if conc != 4 {
		t.Errorf("concurrency=%d want 4（越界归一）", conc)
	}
	if len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Errorf("hours=%v want [10 20]（快照隔离外部改动）", got)
	}
	if p.autoConcurrency() != 4 {
		t.Errorf("autoConcurrency=%d want 4", p.autoConcurrency())
	}

	p.SetTaskAuto(false, nil, 0)
	on, got, conc = p.autoSnapshot()
	if on {
		t.Error("enabled want false")
	}
	if len(got) != 0 {
		t.Errorf("hours=%v want empty", got)
	}
	if conc != 1 || p.autoConcurrency() != 1 {
		t.Errorf("concurrency=%d/%d want 1（未配置回落单号串行）", conc, p.autoConcurrency())
	}
}

// autoStatus 请求自动执行状态接口并解 JSON。
func autoStatus(t *testing.T, p *Panel) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/tasks/auto", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	return got
}

// TestTasksAutoStatusEndpoint 自动执行状态接口：无 Scheduler（纯转发/测试装配）时
// 也必须正常返回基本字段，不得 500。
func TestTasksAutoStatusEndpoint(t *testing.T) {
	p := newTestPanel()
	p.SetTaskAuto(true, []int{10, 20}, 2)
	got := autoStatus(t, p)
	if got["ok"] != true || got["enabled"] != true {
		t.Errorf("ok/enabled=%v/%v want true/true", got["ok"], got["enabled"])
	}
	if conc, _ := got["concurrency"].(float64); conc != 2 {
		t.Errorf("concurrency=%v want 2", got["concurrency"])
	}
	if _, has := got["next_at"]; has {
		t.Error("nil Scheduler 不应给出 next_at")
	}
}

// TestTasksAutoStatusNextAt 装配 Scheduler 且开关打开时给出下次执行时刻；
// 关闭后不再给出（面板据此不显示"下次"）。
func TestTasksAutoStatusNextAt(t *testing.T) {
	sch := scheduler.New(scheduler.Config{TaskAutoHours: []int{10, 20}, TaskAutoFn: func() {}})
	p := New(Config{Version: "test", APIKey: "test-key", Scheduler: sch})
	p.SetTaskAuto(true, []int{10, 20}, 1)
	got := autoStatus(t, p)
	if s, _ := got["next_at"].(string); s == "" {
		t.Fatalf("enabled 状态下 next_at 应为非空时刻：%v", got)
	}
	p.SetTaskAuto(false, []int{10, 20}, 1)
	if _, has := autoStatus(t, p)["next_at"]; has {
		t.Error("关闭后不应再给 next_at")
	}
}

// autoQueueTestPanel 构造一个指向 mock 上游的面板（单 CN 账号），供自动队列端到端测试用。
func autoQueueTestPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up, Scheduler: sch})
	p.SetTaskAuto(true, []int{10, 20}, 1)
	return p
}

// queueSnapshot 读队列状态快照。
func queueSnapshot(p *Panel) (running bool, msg string, items []queueItem) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running, q.lastMsg, append([]queueItem(nil), q.items...)
}

// TestRunAutoQueueNowNoPendingIsReadOnly 无待办时自动轮次**零写操作**：
// 只做只读扫描，不发起任何 accept/claim/上报——"没任务就别打扰上游"是自动化的
// 基本承诺（否则每轮都会白打一批写请求）。
func TestRunAutoQueueNowNoPendingIsReadOnly(t *testing.T) {
	var mu sync.Mutex
	gets, writes := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodGet {
			gets++
		} else {
			writes++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"data":{"tasks":[],"in_period":false}}`)
	}))
	defer srv.Close()

	p := autoQueueTestPanel(t, srv)
	p.RunAutoQueueNow()

	mu.Lock()
	defer mu.Unlock()
	if writes != 0 {
		t.Errorf("无待办时不得发起任何写请求，got %d", writes)
	}
	if gets == 0 {
		t.Error("应至少扫描一次任务列表")
	}
	running, msg, items := queueSnapshot(p)
	if running {
		t.Error("无待办时不应进入运行态")
	}
	if msg != "无待办任务" {
		t.Errorf("lastMsg=%q want 无待办任务", msg)
	}
	if len(items) != 0 {
		t.Errorf("items=%v want empty", items)
	}
}

// TestRunAutoQueueNowExecutesPending 有待办时自动轮次真的执行并回写状态：
// 扫描 → 入队 → 跑动作 → 达标自动领奖 → 条目落 done。
func TestRunAutoQueueNowExecutesPending(t *testing.T) {
	var mu sync.Mutex
	bareListCalls, posts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		if r.Method != http.MethodGet {
			posts++
			mu.Unlock()
			io.WriteString(w, `{"code":0,"data":{"credit":100,"energy":5}}`)
			return
		}
		// 默认口径（无 mp 头）的列表：第一次返回未达标（待办），之后返回已达标
		// （模拟上游异步计分落定）——领奖回读因此立刻命中 claimable。
		if r.URL.Path == "/v2/activity/growth/tasks" && r.Header.Get("X-Client-Platform") != "miniprogram" {
			bareListCalls++
			cur := 0
			if bareListCalls > 1 {
				cur = 5
			}
			mu.Unlock()
			fmt.Fprintf(w, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","title":"对话 5 次","target":5,"current":%d,"accept_status":"accepted"}]}}`, cur)
			return
		}
		mu.Unlock()
		io.WriteString(w, `{"code":0,"data":{"tasks":[],"in_period":false}}`)
	}))
	defer srv.Close()

	p := autoQueueTestPanel(t, srv)
	p.RunAutoQueueNow()

	mu.Lock()
	postCount := posts
	mu.Unlock()
	if postCount == 0 {
		t.Error("有待办时必须真的发起任务动作（accept/claim），实际 0 次写请求")
	}

	running, _, items := queueSnapshot(p)
	if running {
		t.Error("轮次结束后不应仍处运行态")
	}
	if len(items) != 1 || items[0].Code != "chat_5" || items[0].Kind != "growth" {
		t.Fatalf("items=%+v want 单个 chat_5 条目", items)
	}
	if items[0].Status != "done" {
		t.Errorf("status=%s message=%q want done（自动执行应跑完并回写状态）", items[0].Status, items[0].Message)
	}
}

// TestRunAutoQueueNowSkipsWhileRunning 队列已在跑时自动轮次直接跳过（不排队堆积）。
func TestRunAutoQueueNowSkipsWhileRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"data":{"tasks":[],"in_period":false}}`)
	}))
	defer srv.Close()

	p := autoQueueTestPanel(t, srv)
	q := p.queue()
	if !q.begin() {
		t.Fatal("预占启动权应成功")
	}
	p.RunAutoQueueNow() // 应立刻返回、不扫描、不改状态
	if _, _, items := queueSnapshot(p); len(items) != 0 {
		t.Errorf("跳过轮次不得写入队列条目：%+v", items)
	}
	q.abort()
}
