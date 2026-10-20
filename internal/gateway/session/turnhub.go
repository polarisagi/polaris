package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"
)

// TurnHub 回合注册表（ADR-0116 会话回合与连接解耦）：按 sessionID 持有当前/最近一次
// 回合的有界事件缓冲与订阅者，使回合生命周期独立于发起它的 HTTP 连接。
//
// 为什么存在：此前回合 ctx 直接取自请求 ctx，浏览器切会话/关页面即中断连接，
// runFSMTurn 的 ctx.Done 分支随即 Interrupt(Abort) 掉 Agent（原 GD-13-002「断连即
// 中止」）。现回合 ctx 由本注册表持有，仅显式中止（Abort）或守护进程关停（Shutdown）
// 才会取消；连接只是订阅者，断开只退订。
//
// 纯内存：重启即丢失（回合本身也随进程消亡，已落库的消息不受影响，HE-6 不涉及）。
// 并发模型：锁序恒为 hub.mu → turn.mu，反向持有会死锁，新增代码须遵守。
type TurnHub struct {
	cfg TurnHubConfig

	mu        sync.Mutex
	bySession map[string]*Turn
	live      map[*Turn]struct{}
	closed    bool
	wg        sync.WaitGroup
}

// TurnHubConfig 缓冲与保留参数；零值字段取默认。
type TurnHubConfig struct {
	// MaxEvents 单回合缓冲事件条数上限（默认 4096）。
	MaxEvents int
	// MaxBytes 单回合缓冲字节上限（默认 4MB，按文本与 payload JSON 长度估算）。
	MaxBytes int
	// Retention 回合结束后保留缓冲供晚到重连回放的时长（默认 5 分钟）。
	Retention time.Duration
}

const (
	defaultTurnMaxEvents = 4096
	defaultTurnMaxBytes  = 4 << 20
	defaultTurnRetention = 5 * time.Minute
	// compactKeepTail 压缩时保留不合并的最新事件数：实时订阅者通常就在尾部附近，
	// 尾部不动能让它们不会碰到被合并事件。
	compactKeepTail = 64
)

// BeginResult 是 TurnHub.Begin 的结果。
type BeginResult int

const (
	BeginOK     BeginResult = iota
	BeginBusy               // 该会话已有运行中的回合
	BeginClosed             // 注册表已关停
)

// NewTurnHub 构造注册表。
func NewTurnHub(cfg TurnHubConfig) *TurnHub {
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = defaultTurnMaxEvents
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultTurnMaxBytes
	}
	if cfg.Retention <= 0 {
		cfg.Retention = defaultTurnRetention
	}
	return &TurnHub{
		cfg:       cfg,
		bySession: make(map[string]*Turn),
		live:      make(map[*Turn]struct{}),
	}
}

// SeqEvent 带序号的缓冲事件。压缩合并后的事件 Seq 取区间末尾、FirstSeq 取区间起点。
type SeqEvent struct {
	Seq      int64
	FirstSeq int64
	Event    Event
}

// Turn 一次回合的事件缓冲与订阅者集合；同时实现 Sink 供 RunTurn 写入。
type Turn struct {
	hub    *TurnHub
	ID     string
	cancel context.CancelFunc

	// replaced/timer 由 hub.mu 保护。
	replaced bool
	timer    *time.Timer

	mu        sync.Mutex
	sessionID string
	startedAt time.Time
	done      bool
	terminal  bool
	events    []SeqEvent
	bytes     int
	nextSeq   int64
	dropped   int
	subs      map[*Subscription]struct{}
}

// Begin 登记一个新回合。sessionID 为空表示新会话，待 RunTurn 经 KindSession 事件
// 回报后再绑定。cancel 是回合 ctx 的取消函数，由 Abort/Shutdown 调用。
func (h *TurnHub) Begin(sessionID string, cancel context.CancelFunc) (*Turn, BeginResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, BeginClosed
	}
	if sessionID != "" {
		if old := h.bySession[sessionID]; old != nil {
			if !old.isDone() {
				return nil, BeginBusy
			}
			h.dropLocked(old)
		}
	}
	t := &Turn{
		hub:       h,
		ID:        newTurnID(),
		cancel:    cancel,
		sessionID: sessionID,
		startedAt: time.Now(),
		subs:      make(map[*Subscription]struct{}),
	}
	h.wg.Add(1)
	h.live[t] = struct{}{}
	if sessionID != "" {
		h.bySession[sessionID] = t
	}
	return t, BeginOK
}

func newTurnID() string { return "turn_" + newSessionID()[len("sess_"):] }

// dropLocked 把已结束的回合立即移出注册表（调用方持 hub.mu）。
func (h *TurnHub) dropLocked(t *Turn) {
	t.replaced = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	delete(h.live, t)
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if h.bySession[sid] == t {
		delete(h.bySession, sid)
	}
}

// bind 把回合登记到会话 ID 下（新会话在 RunTurn 确定 ID 后调用）。
func (h *TurnHub) bind(t *Turn, sessionID string) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.mu.Lock()
	already := t.sessionID != ""
	if !already {
		t.sessionID = sessionID
	}
	t.mu.Unlock()
	if already {
		return
	}
	if old := h.bySession[sessionID]; old != nil && old != t && !old.isDone() {
		slog.Warn("session: turnhub bind collides with running turn", "session", sessionID)
		return
	}
	h.bySession[sessionID] = t
}

// finished 回合收尾：释放等待计数并启动保留期计时器。
func (h *TurnHub) finished(t *Turn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wg.Done()
	if t.replaced || h.closed {
		return
	}
	t.timer = time.AfterFunc(h.cfg.Retention, func() { h.expire(t) })
}

func (h *TurnHub) expire(t *Turn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t.replaced {
		return
	}
	h.dropLocked(t)
}

// Get 返回会话当前或保留期内的回合；无则 nil。
func (h *TurnHub) Get(sessionID string) *Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bySession[sessionID]
}

// Running 会话是否有运行中的回合。
func (h *TurnHub) Running(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.bySession[sessionID]
	return t != nil && !t.isDone()
}

// RunningSet 当前所有运行中回合的会话 ID 集合（供会话列表一次性标注 running）。
func (h *TurnHub) RunningSet() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]bool, len(h.bySession))
	for sid, t := range h.bySession {
		if !t.isDone() {
			out[sid] = true
		}
	}
	return out
}

// Abort 显式中止会话的运行中回合：取消回合 ctx，由 runFSMTurn 的 ctx.Done 分支
// 向 Agent 内核发 Interrupt(Abort)。返回是否确有运行中回合被中止。
func (h *TurnHub) Abort(sessionID string) bool {
	h.mu.Lock()
	t := h.bySession[sessionID]
	h.mu.Unlock()
	if t == nil || t.isDone() {
		return false
	}
	t.cancel()
	return true
}

// Shutdown 关停：拒绝新回合、取消全部运行中回合，并等待它们收尾（或 ctx 超时）。
func (h *TurnHub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	for t := range h.live {
		if !t.isDone() {
			t.cancel()
		}
	}
	h.mu.Unlock()

	waited := make(chan struct{})
	concurrent.SafeGo(ctx, "turnhub.shutdown_wait", func(context.Context) {
		h.wg.Wait()
		close(waited)
	})
	select {
	case <-waited:
	case <-ctx.Done():
		slog.Warn("session: turnhub shutdown timed out waiting for turns")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for t := range h.live {
		if t.timer != nil {
			t.timer.Stop()
			t.timer = nil
		}
	}
}

// ── Turn ────────────────────────────────────────────────────────────────

// SessionID 回合所属会话（新会话在 KindSession 之前为空）。
func (t *Turn) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *Turn) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// Done 回合是否已结束。
func (t *Turn) Done() bool { return t.isDone() }

// Emit 实现 Sink：写入缓冲并唤醒订阅者。永不返回错误（纯内存）。
func (t *Turn) Emit(ev Event) error {
	if ev.Kind == KindSession {
		sid, _ := ev.Payload["session_id"].(string)
		t.hub.bind(t, sid)
		ev.Payload = map[string]any{"session_id": sid, "turn_id": t.ID}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	t.appendLocked(ev)
	return nil
}

// Finish 标记回合结束。若回合从未产出终止事件（complete/error，例如被中止），
// 补一条 complete{aborted:true}，保证每个订阅者都以终止事件收尾。幂等。
func (t *Turn) Finish() {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	if !t.terminal {
		t.appendLocked(Event{Kind: KindComplete, Payload: map[string]any{
			"session_id": t.sessionID,
			"aborted":    true,
		}})
	}
	t.done = true
	t.wakeLocked()
	t.mu.Unlock()
	t.hub.finished(t)
}

func (t *Turn) wakeLocked() {
	for s := range t.subs {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (t *Turn) appendLocked(ev Event) {
	t.nextSeq++
	t.events = append(t.events, SeqEvent{Seq: t.nextSeq, FirstSeq: t.nextSeq, Event: ev})
	t.bytes += eventSize(ev)
	if ev.Kind == KindComplete || ev.Kind == KindError {
		t.terminal = true
	}
	if len(t.events) > t.hub.cfg.MaxEvents || t.bytes > t.hub.cfg.MaxBytes {
		t.compactLocked()
	}
	t.wakeLocked()
}

func eventSize(ev Event) int {
	n := len(ev.Text) + 32
	if len(ev.Payload) > 0 {
		if b, err := json.Marshal(ev.Payload); err == nil {
			n += len(b)
		}
	}
	return n
}

func mergeable(k EventKind) bool { return k == KindDelta || k == KindReasoning }

// compactLocked 缓冲越界时的有界化：先把较旧部分里相邻同类的 Delta/Reasoning 合并
// 成一条（文本不丢），仍越界再从最旧端丢弃直至回落到上限的 3/4。
// 被合并事件的 FirstSeq..Seq 区间内曾收到过部分内容的订阅者会跳过该条（见 Poll），
// 宁可缺一段也不重复输出。
func (t *Turn) compactLocked() {
	cut := len(t.events) - compactKeepTail
	if cut < 2 {
		cut = len(t.events)
	}
	out := make([]SeqEvent, 0, len(t.events))
	var run strings.Builder
	flush := func() {
		if len(out) > 0 && run.Len() > 0 {
			out[len(out)-1].Event.Text = run.String()
		}
		run.Reset()
	}
	for i := 0; i < cut; i++ {
		e := t.events[i]
		if n := len(out); n > 0 && mergeable(e.Event.Kind) && out[n-1].Event.Kind == e.Event.Kind {
			out[n-1].Seq = e.Seq
			run.WriteString(e.Event.Text)
			continue
		}
		flush()
		out = append(out, e)
		run.Reset()
		if mergeable(e.Event.Kind) {
			run.WriteString(e.Event.Text)
		}
	}
	flush()
	out = append(out, t.events[cut:]...)
	t.events = out

	t.bytes = 0
	for _, e := range t.events {
		t.bytes += eventSize(e.Event)
	}
	maxE, maxB := t.hub.cfg.MaxEvents, t.hub.cfg.MaxBytes
	if len(t.events) <= maxE && t.bytes <= maxB {
		return
	}
	drop := 0
	for drop < len(t.events)-1 && (len(t.events)-drop > maxE*3/4 || t.bytes > maxB*3/4) {
		t.bytes -= eventSize(t.events[drop].Event)
		drop++
	}
	t.events = append([]SeqEvent(nil), t.events[drop:]...)
	t.dropped += drop
}

// ── Subscription ─────────────────────────────────────────────────────────

// Subscription 回合事件订阅游标。非线程安全（单消费者）。
type Subscription struct {
	t      *Turn
	cursor int64
	wake   chan struct{}
}

// Subscribe 订阅回合，从 seq>after 的缓冲事件开始回放，之后接实时事件。
func (t *Turn) Subscribe(after int64) *Subscription {
	s := &Subscription{t: t, cursor: after, wake: make(chan struct{}, 1)}
	t.mu.Lock()
	t.subs[s] = struct{}{}
	t.mu.Unlock()
	return s
}

// Wake 有新事件或回合结束时可读。
func (s *Subscription) Wake() <-chan struct{} { return s.wake }

// Close 退订。幂等；退订不影响回合运行。
func (s *Subscription) Close() {
	s.t.mu.Lock()
	delete(s.t.subs, s)
	s.t.mu.Unlock()
}

// Poll 取下一条事件。ok=false 且 done=true 表示回合已结束且事件已读完；
// ok=false 且 done=false 表示暂无新事件，等待 Wake。
func (s *Subscription) Poll() (ev SeqEvent, ok, done bool) {
	t := s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		idx := sort.Search(len(t.events), func(i int) bool { return t.events[i].Seq > s.cursor })
		if idx >= len(t.events) {
			return SeqEvent{}, false, t.done
		}
		e := t.events[idx]
		prev := s.cursor
		s.cursor = e.Seq
		if e.FirstSeq <= prev {
			// 合并事件且订阅者已读过其中一部分：整条跳过，避免重复输出。
			continue
		}
		return e, true, false
	}
}
