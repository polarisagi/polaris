package session

// 回合步骤时间线（ADR-0118）。
//
// 内核流事件（思考、工具调用/结果、审批、降级提示、LLM 调用、子智能体事件）在这里转成结构化的
// `step` 事件：实时经 Sink 推给订阅者（进 TurnHub 缓冲，断线重连可回放），同时按有界规则
// 增量落库 chat_turn_steps。步骤**只给人看**：从不写进模型上下文，不增加 token。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// 步骤 kind / phase / status 取值（wire 契约，见 M13 SSE 协议）。
const (
	StepKindThinking = "thinking"
	StepKindTool     = "tool"
	StepKindSubagent = "subagent"
	StepKindApproval = "approval"
	StepKindRetry    = "retry"
	StepKindNotice   = "notice"
	StepKindLLM      = "llm"

	stepPhaseStart  = "start"
	stepPhaseUpdate = "update"
	stepPhaseEnd    = "end"

	StepStatusRunning   = "running"
	StepStatusOK        = "ok"
	StepStatusError     = "error"
	StepStatusDenied    = "denied"
	StepStatusCancelled = "cancelled"

	// 摘要行：每个回合恒有一行 seq=0、kind=summary，列表接口只读它，不必扫全部步骤。
	stepKindSummary = "summary"
)

// 预览与落库上限（字符数按 rune 计，字节上限按存储计）。
const (
	argsPreviewMax      = 300
	resultPreviewMax    = 600
	thinkingPersistMax  = 8 << 10 // 单个思考步骤落库截断
	thinkingEmitEvery   = 150 * time.Millisecond
	maxPersistHeadSteps = 200
	maxPersistHeadBytes = 160 << 10
	maxPersistTailSteps = 80
	maxPersistTailBytes = 64 << 10
	writerBatch         = 20
	writerFlushEvery    = 2 * time.Second
	writerQueue         = 512
)

// StepTokens 步骤关联的 token 用量（仅 llm 步骤有值）。
type StepTokens struct {
	In       int `json:"in"`
	Out      int `json:"out"`
	CacheHit int `json:"cache_hit"`
}

// Step 是一个步骤的完整状态，既是 wire 载荷也是落库内容（phase 字段只在 wire 上有意义）。
type Step struct {
	ID            string      `json:"id"`
	ParentID      string      `json:"parent_id,omitempty"`
	Seq           int         `json:"seq"`
	Kind          string      `json:"kind"`
	Phase         string      `json:"phase,omitempty"`
	Title         string      `json:"title"`
	Tool          string      `json:"tool,omitempty"`
	ArgsPreview   string      `json:"args_preview,omitempty"`
	ResultPreview string      `json:"result_preview,omitempty"`
	Status        string      `json:"status"`
	StartedAt     int64       `json:"started_at"` // Unix 毫秒
	DurationMs    int64       `json:"duration_ms"`
	Tokens        *StepTokens `json:"tokens,omitempty"`
	// Content 仅 thinking 的 end 事件与落库行携带（≤8KB）；Delta 仅 thinking 的 update 事件携带。
	Content string `json:"content,omitempty"`
	Delta   string `json:"delta,omitempty"`
}

// StepRecord 是落库行。
type StepRecord struct {
	Seq         int
	ParentID    string
	Kind        string
	Data        []byte
	CreatedAtMs int64
}

// TurnUsageSummary 一个回合的 LLM 用量汇总（来自 llm_calls）。
type TurnUsageSummary struct {
	Calls    int64
	Input    int64
	CacheHit int64
	Output   int64
	CostUSD  float64
}

// TurnStore 回合步骤与元数据的持久化（可选依赖：nil 时步骤只实时推送、不落库）。
// 接口在消费方定义（HE-3）。
type TurnStore interface {
	AppendTurnSteps(ctx context.Context, sessionID, turnID string, recs []StepRecord) error
	TurnUsage(ctx context.Context, turnID string) (TurnUsageSummary, error)
	AttachTurnMeta(ctx context.Context, messageID int64, turnID, usageJSON string) error
}

// StepSummary 回合步骤摘要（complete 事件与落库 summary 行共用）。
type StepSummary struct {
	Total      int        `json:"steps"`
	Omitted    int        `json:"omitted,omitempty"`
	Tools      int        `json:"tools"`
	Searches   int        `json:"searches"`
	Fetches    int        `json:"fetches"`
	DurationMs int64      `json:"duration_ms"`
	Tokens     StepTokens `json:"tokens"`
}

type stepState struct {
	Step
	scope   string
	content strings.Builder
	pending strings.Builder // 尚未推出的思考增量
	lastEmt time.Time
	done    bool
}

// stepRecorder 单回合使用，非并发安全（runFSMTurn 事件循环单协程驱动）。
type stepRecorder struct {
	sink      Sink
	store     TurnStore
	sessionID string
	turnID    string
	now       func() time.Time
	startedAt time.Time

	seq       int
	byID      map[string]*stepState
	byCall    map[string]*stepState
	thinking  map[string]*stepState // scope → 当前思考步骤
	approvals []*stepState
	agents    map[string]*stepState // agentID → 子智能体步骤
	order     []*stepState          // 创建顺序，finalize 收尾用

	sum StepSummary

	// 落库有界化：头部直写，其后进尾部环形缓冲。
	writer    *stepWriter
	headN     int
	headBytes int
	tail      []StepRecord
	tailBytes int
	dropped   int
}

func newStepRecorder(ctx context.Context, sink Sink, store TurnStore, sessionID, turnID string) *stepRecorder {
	r := &stepRecorder{
		sink: sink, store: store, sessionID: sessionID, turnID: turnID,
		now: time.Now, byID: map[string]*stepState{}, byCall: map[string]*stepState{},
		thinking: map[string]*stepState{}, agents: map[string]*stepState{},
	}
	r.startedAt = r.now()
	if store != nil {
		r.writer = newStepWriter(ctx, store, sessionID, turnID)
	}
	return r
}

func (r *stepRecorder) ms() int64 { return r.now().UnixMilli() }

// scopeOf 返回事件所属的执行域：主智能体为 ""，子智能体为其 AgentID。
func scopeOf(ev types.AgentStreamEvent) string {
	if ev.IsNested || ev.ParentID != "" {
		return ev.AgentID
	}
	return ""
}

// ensureSubagent 为首次出现的子智能体建立 subagent 步骤，返回其步骤 ID（主智能体返回 ""）。
func (r *stepRecorder) ensureSubagent(ev types.AgentStreamEvent) string {
	scope := scopeOf(ev)
	if scope == "" {
		return ""
	}
	if st, ok := r.agents[scope]; ok {
		return st.ID
	}
	parent := ""
	if p, ok := r.agents[ev.ParentID]; ok {
		parent = p.ID
	}
	title := "子智能体"
	if ev.ChildAgentRole != "" {
		title += " · " + ev.ChildAgentRole
	}
	st := r.newStep("agent-"+scope, parent, StepKindSubagent, title, scope)
	r.agents[scope] = st
	r.emit(st, stepPhaseStart)
	return st.ID
}

func (r *stepRecorder) newStep(id, parent, kind, title, scope string) *stepState {
	r.seq++
	st := &stepState{scope: scope}
	st.ID, st.ParentID, st.Seq, st.Kind, st.Title = id, parent, r.seq, kind, title
	st.Status, st.StartedAt = StepStatusRunning, r.ms()
	r.byID[id] = st
	r.order = append(r.order, st)
	return st
}

func (r *stepRecorder) emit(st *stepState, phase string) {
	if r.sink == nil {
		return
	}
	s := st.Step
	s.Phase = phase
	switch phase {
	case stepPhaseEnd:
		if s.Kind == StepKindThinking {
			s.Content = clipBytes(st.content.String(), thinkingPersistMax)
		}
	case stepPhaseUpdate:
		s.Delta = st.pending.String()
		st.pending.Reset()
		st.lastEmt = r.now()
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return
	}
	_ = r.sink.Emit(Event{Kind: KindStep, Payload: payload})
}

// end 结束一个步骤：写终态、推 end 事件、落库、累计摘要。幂等。
func (r *stepRecorder) end(st *stepState, status string) {
	if st.done {
		return
	}
	st.done = true
	st.Status = status
	if st.DurationMs == 0 {
		st.DurationMs = max(r.ms()-st.StartedAt, 0)
	}
	if st.Kind == StepKindThinking && st.pending.Len() > 0 {
		r.emit(st, stepPhaseUpdate)
	}
	r.emit(st, stepPhaseEnd)
	r.persist(st)
	switch st.Kind {
	case StepKindTool:
		r.sum.Tools++
		switch st.Tool {
		case "web_search":
			r.sum.Searches++
		case "web_fetch":
			r.sum.Fetches++
		}
	case StepKindLLM:
		if st.Tokens != nil {
			r.sum.Tokens.In += st.Tokens.In
			r.sum.Tokens.Out += st.Tokens.Out
			r.sum.Tokens.CacheHit += st.Tokens.CacheHit
		}
	}
	r.sum.Total++
	if r.thinking[st.scope] == st {
		delete(r.thinking, st.scope)
	}
}

func (r *stepRecorder) instant(kind, title, scopeParent, status string, mutate func(*stepState)) *stepState {
	st := r.newStep(fmt.Sprintf("%s-%d", kind, r.seq+1), scopeParent, kind, title, "")
	if mutate != nil {
		mutate(st)
	}
	r.emit(st, stepPhaseStart)
	r.end(st, status)
	return st
}

func (r *stepRecorder) closeThinking(scope string) {
	if st, ok := r.thinking[scope]; ok {
		r.end(st, StepStatusOK)
	}
}

// onEvent 消费一条内核流事件。
func (r *stepRecorder) onEvent(ev types.AgentStreamEvent) {
	parent := r.ensureSubagent(ev)
	scope := scopeOf(ev)
	if ev.Type == types.AgentStreamEventThinking {
		r.onThinking(ev, parent, scope)
		return
	}
	switch ev.Type {
	case types.AgentStreamEventToolCall, types.AgentStreamEventToolResult, types.AgentStreamEventToken,
		types.AgentStreamEventLLM, types.AgentStreamEventApproval, types.AgentStreamEventNotice:
		r.closeThinking(scope)
	}
	switch ev.Type {
	case types.AgentStreamEventToolCall:
		r.onToolCall(ev, parent, scope)
	case types.AgentStreamEventToolResult:
		r.onToolResult(ev, parent, scope)
	case types.AgentStreamEventApproval:
		st := r.newStep(fmt.Sprintf("ap-%d", r.seq+1), parent, StepKindApproval, "等待审批："+ev.ToolName, scope)
		st.Tool, st.ArgsPreview = ev.ToolName, clipRunes(string(ev.ToolInput), argsPreviewMax)
		r.approvals = append(r.approvals, st)
		r.emit(st, stepPhaseStart)
	case types.AgentStreamEventNotice:
		r.instant(StepKindNotice, clipRunes(ev.Content, 200), parent, StepStatusOK, nil)
	case types.AgentStreamEventLLM:
		if ev.Retry {
			r.instant(StepKindRetry, "模型空输出，追加一次恢复调用", parent, StepStatusOK, nil)
		}
		r.instant(StepKindLLM, "LLM 调用 · "+ev.ToolName, parent, StepStatusOK, func(st *stepState) {
			st.DurationMs = ev.DurationMs
			st.StartedAt = r.ms() - ev.DurationMs
			st.Tokens = &StepTokens{In: ev.InTokens, Out: ev.OutTokens, CacheHit: ev.CacheHitTokens}
		})
	}
}

func (r *stepRecorder) onThinking(ev types.AgentStreamEvent, parent, scope string) {
	st, ok := r.thinking[scope]
	if !ok {
		st = r.newStep(fmt.Sprintf("th-%d", r.seq+1), parent, StepKindThinking, "思考", scope)
		r.thinking[scope] = st
		r.emit(st, stepPhaseStart)
	}
	st.content.WriteString(ev.Content)
	st.pending.WriteString(ev.Content)
	if r.now().Sub(st.lastEmt) >= thinkingEmitEvery {
		r.emit(st, stepPhaseUpdate)
	}
}

func (r *stepRecorder) onToolCall(ev types.AgentStreamEvent, parent, scope string) {
	id := ev.CallID
	if id == "" {
		id = fmt.Sprintf("n%d", r.seq+1)
	}
	st := r.newStep("tool-"+id, parent, StepKindTool, ev.ToolName, scope)
	st.Tool = ev.ToolName
	st.ArgsPreview = argsPreview(ev.ToolName, ev.ToolInput)
	r.byCall[scope+"|"+id] = st
	r.emit(st, stepPhaseStart)
}

func (r *stepRecorder) onToolResult(ev types.AgentStreamEvent, parent, scope string) {
	st := r.findToolStep(ev, scope)
	if st == nil {
		st = r.newStep(fmt.Sprintf("tool-r%d", r.seq+1), parent, StepKindTool, ev.ToolName, scope)
		st.Tool = ev.ToolName
		r.emit(st, stepPhaseStart)
	}
	raw := ev.ResultRaw
	if raw == "" {
		raw = ev.Content
	}
	isErr := ev.IsError || looksLikeToolError(raw)
	st.ResultPreview = previewToolResult(ev.ToolName, raw, isErr)
	if ev.DurationMs > 0 {
		st.DurationMs = ev.DurationMs
		st.StartedAt = r.ms() - ev.DurationMs
	}
	status := StepStatusOK
	if isErr {
		status = StepStatusError
		if looksDenied(raw) {
			status = StepStatusDenied
		}
	}
	r.settleApproval(ev.ToolName, scope, status)
	r.end(st, status)
}

// findToolStep 按 CallID 找对应的工具步骤；无 CallID 或错过开始事件时按同工具最早的未结束步骤配对。
func (r *stepRecorder) findToolStep(ev types.AgentStreamEvent, scope string) *stepState {
	if ev.CallID != "" {
		if st := r.byCall[scope+"|"+ev.CallID]; st != nil {
			return st
		}
	}
	for _, c := range r.order {
		if c.Kind == StepKindTool && !c.done && c.Tool == ev.ToolName && c.scope == scope {
			return c
		}
	}
	return nil
}

// settleApproval 让与该工具配对的审批步骤随工具结果落定（被拒则记 denied，否则 ok）。
func (r *stepRecorder) settleApproval(tool, scope, toolStatus string) {
	for i, ap := range r.approvals {
		if ap == nil || ap.Tool != tool || ap.scope != scope {
			continue
		}
		ap.DurationMs = max(r.ms()-ap.StartedAt, 0)
		if toolStatus == StepStatusDenied {
			r.end(ap, StepStatusDenied)
		} else {
			r.end(ap, StepStatusOK)
		}
		r.approvals[i] = nil
		return
	}
}

// finalize 回合收尾：把未结束的步骤收口，落库尾部与摘要行，返回摘要。aborted 时未完成的工具记 cancelled。
func (r *stepRecorder) finalize(aborted bool) StepSummary {
	rest := StepStatusOK
	if aborted {
		rest = StepStatusCancelled
	}
	for _, st := range r.order {
		if st.done || st.Kind == StepKindSubagent {
			continue
		}
		r.end(st, rest)
	}
	for _, st := range r.order { // 子智能体最后收口，时长取到此刻
		if !st.done {
			r.end(st, rest)
		}
	}
	r.sum.DurationMs = max(r.ms()-r.startedAt.UnixMilli(), 0)
	r.sum.Omitted = r.dropped
	if r.writer != nil {
		for _, rec := range r.tail {
			r.writer.push(rec)
		}
		data, _ := json.Marshal(r.sum)
		r.writer.push(StepRecord{Seq: 0, Kind: stepKindSummary, Data: data, CreatedAtMs: r.ms()})
		r.writer.close(3 * time.Second)
	}
	return r.sum
}

func (r *stepRecorder) persist(st *stepState) {
	if r.writer == nil {
		return
	}
	s := st.Step
	s.Phase = ""
	if s.Kind == StepKindThinking {
		s.Content = clipBytes(st.content.String(), thinkingPersistMax)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	rec := StepRecord{Seq: s.Seq, ParentID: s.ParentID, Kind: s.Kind, Data: data, CreatedAtMs: r.ms()}
	if r.headN < maxPersistHeadSteps && r.headBytes+len(data) <= maxPersistHeadBytes {
		r.headN++
		r.headBytes += len(data)
		r.writer.push(rec)
		return
	}
	r.tail = append(r.tail, rec)
	r.tailBytes += len(data)
	for len(r.tail) > maxPersistTailSteps || r.tailBytes > maxPersistTailBytes {
		r.tailBytes -= len(r.tail[0].Data)
		r.tail = r.tail[1:]
		r.dropped++
	}
}

// ── 批量异步写入 ─────────────────────────────────────────────────────────

// stepWriter 把步骤行批量写库，不阻塞回合：回合协程只往带缓冲的通道里塞，满了就丢并计数
// （步骤是展示数据，丢几行不影响回合正确性）。
type stepWriter struct {
	ch      chan StepRecord
	done    chan struct{}
	once    sync.Once
	dropped int
}

func newStepWriter(ctx context.Context, store TurnStore, sessionID, turnID string) *stepWriter {
	w := &stepWriter{ch: make(chan StepRecord, writerQueue), done: make(chan struct{})}
	wctx := context.WithoutCancel(ctx)
	concurrent.SafeGo(wctx, "session.step_writer", func(ctx context.Context) {
		defer close(w.done)
		tick := time.NewTicker(writerFlushEvery)
		defer tick.Stop()
		var batch []StepRecord
		flush := func() {
			if len(batch) == 0 {
				return
			}
			fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := store.AppendTurnSteps(fctx, sessionID, turnID, batch); err != nil {
				slogWarn("session: persist turn steps failed", sessionID, turnID, err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case rec, ok := <-w.ch:
				if !ok {
					flush()
					return
				}
				batch = append(batch, rec)
				if len(batch) >= writerBatch {
					flush()
				}
			case <-tick.C:
				flush()
			case <-ctx.Done():
				flush()
				return
			}
		}
	})
	return w
}

func (w *stepWriter) push(rec StepRecord) {
	select {
	case w.ch <- rec:
	default:
		w.dropped++
	}
}

func (w *stepWriter) close(timeout time.Duration) {
	w.once.Do(func() { close(w.ch) })
	select {
	case <-w.done:
	case <-time.After(timeout):
	}
}

// ── 小工具 ───────────────────────────────────────────────────────────────

func slogWarn(msg, sessionID, turnID string, err error) {
	slog.Warn(msg, "session", sessionID, "turn", turnID, "err", err)
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// clipBytes 按字节上限截断并保持 UTF-8 完整，超出时追加提示。
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…（已截断）"
}
