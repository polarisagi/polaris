package session

import (
	"context"
	"sync"
	"testing"
	"time"

	gwtypes "github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── 窄接口 fake 实现（HE-3：与 session 包声明的接口逐一对应）──────────────

type fakeHooks struct {
	mu          sync.Mutex
	fired       []string
	blockBefore map[string]string // event -> reason，非空表示 FireBefore 该 event 返回 blocked=true
}

func (h *fakeHooks) Fire(event string, env map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fired = append(h.fired, event)
}

func (h *fakeHooks) FireBefore(event string, env map[string]string) (bool, string) {
	if h.blockBefore != nil {
		if reason, ok := h.blockBefore[event]; ok {
			return true, reason
		}
	}
	return false, ""
}

type fakePersistence struct {
	mu       sync.Mutex
	sessions map[string]bool
	history  []types.Message
	saved    []savedMessage
	titled   string
	touched  bool
}

type savedMessage struct {
	sessionID, role, content string
}

func newFakePersistence() *fakePersistence {
	return &fakePersistence{sessions: map[string]bool{}}
}

func (p *fakePersistence) EnsureSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[sessionID] = true
	return nil
}

func (p *fakePersistence) ListMessages(ctx context.Context, sessionID string) ([]types.Message, error) {
	return p.history, nil
}

func (p *fakePersistence) SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, durationMs int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = append(p.saved, savedMessage{sessionID, role, content})
	return nil
}

func (p *fakePersistence) UpdateSessionTitle(ctx context.Context, sessionID, firstInput string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.titled = firstInput
	return nil
}

func (p *fakePersistence) TouchSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touched = true
	return nil
}

func (p *fakePersistence) SampleAndScoreReply(sessionID, query, response string) {}

func (p *fakePersistence) savedAssistantReplies() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range p.saved {
		if m.role == "assistant" {
			out = append(out, m.content)
		}
	}
	return out
}

type fakePrompt struct{}

func (fakePrompt) InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message {
	return history
}
func (fakePrompt) ReadActivatedSystemPrompt() string { return "" }
func (fakePrompt) ExpandContextRefs(ctx context.Context, input string) (string, []string) {
	return input, nil
}

type fakeCompression struct {
	needsCompact bool
}

func (c *fakeCompression) Stats(msgs []types.Message) gwtypes.ContextStats {
	return gwtypes.ContextStats{UsagePercent: 0}
}
func (c *fakeCompression) WarnPct() float64 { return 80 }
func (c *fakeCompression) NeedsCompact(msgs []types.Message) bool {
	return c.needsCompact
}
func (c *fakeCompression) Compact(ctx context.Context, sessionID string, msgs []types.Message, provider protocol.Provider, mem MemoryFacade) ([]types.Message, gwtypes.CompactResult, error) {
	return msgs, gwtypes.CompactResult{Skipped: true}, nil
}
func (c *fakeCompression) ForceCompact(ctx context.Context, sessionID string, msgs []types.Message, provider protocol.Provider, mem MemoryFacade) ([]types.Message, gwtypes.CompactResult, error) {
	return msgs, gwtypes.CompactResult{Skipped: true}, nil
}

// fakeSlash 默认不处理任何命令（Handled=false），除非 forceHandle 设置。
type fakeSlash struct {
	forceHandle bool
	response    string
}

func (s *fakeSlash) Dispatch(ctx context.Context, input, sessionID string, history []types.Message, provider protocol.Provider, sink Sink, mem MemoryFacade) CommandResult {
	if !s.forceHandle {
		return CommandResult{Handled: false, UpdatedHistory: history}
	}
	_ = sink.Emit(Event{Kind: KindDelta, Text: s.response})
	return CommandResult{Handled: true, Response: s.response, UpdatedHistory: history}
}

type fakeProvider struct{}

func (fakeProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, apperr.New(apperr.CodeInternal, "not implemented in fake")
}
func (fakeProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, apperr.New(apperr.CodeInternal, "not implemented in fake")
}
func (fakeProvider) Capabilities() types.ProviderCapabilities { return types.ProviderCapabilities{} }
func (fakeProvider) Tokenizer() protocol.TokenizerAdapter     { return nil }
func (fakeProvider) ModelID() string                          { return "fake-model" }

type fakeRegistry struct{}

func (fakeRegistry) PickProvider(role string) protocol.Provider                           { return fakeProvider{} }
func (fakeRegistry) PickProviderName(role string) string                                  { return "fake" }
func (fakeRegistry) PickProviderByRecordID(mID string) protocol.Provider                  { return fakeProvider{} }
func (fakeRegistry) UnregisterAll()                                                       {}
func (fakeRegistry) RegisterWithRole(name, displayName, role string, p protocol.Provider) {}

// fakeAgentController 驱动一个可控的 SubscribeStream 事件序列。
type fakeAgentController struct {
	events      chan types.AgentStreamEvent
	interrupted bool
	sendErr     error
}

func newFakeAgentController() *fakeAgentController {
	return &fakeAgentController{events: make(chan types.AgentStreamEvent, 8)}
}

func (a *fakeAgentController) AgentID() string             { return "fake-agent" }
func (a *fakeAgentController) SetTaskIntent(intent []byte) {}
func (a *fakeAgentController) SetSpawnDepth(depth int)     {}
func (a *fakeAgentController) SetMemoryNamespace(ns string) {}
func (a *fakeAgentController) SendIntent(trigger types.AgentTrigger) error {
	return a.sendErr
}
func (a *fakeAgentController) SurpriseIndex() float64        { return 0 }
func (a *fakeAgentController) Memory() protocol.MemoryFacade { return nil }
func (a *fakeAgentController) Interrupt(req types.InterruptRequest) {
	a.interrupted = true
}
func (a *fakeAgentController) SetPreferences(map[string]string)   {}
func (a *fakeAgentController) CurrentState() types.AgentState     { return types.AgentStateIdle }
func (a *fakeAgentController) ConfigInfo() map[string]any         { return nil }
func (a *fakeAgentController) SetMonthlyBudgetUSD(budget float64) {}
func (a *fakeAgentController) SubscribeStream(ctx context.Context) <-chan types.AgentStreamEvent {
	return a.events
}
func (a *fakeAgentController) InjectReplayData(calls []protocol.ReplayLLMCall) {}

// fakeAgentPool：Acquire 返回预置的 fakeAgentController；AcquireHeadless 返回
// 预置的 AgentResult 或 error。
type fakeAgentPool struct {
	ctrl           *fakeAgentController
	acquireErr     error
	headlessResult *types.AgentResult
	headlessErr    error
	releaseCalled  bool
}

func (p *fakeAgentPool) Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error) {
	if p.acquireErr != nil {
		return nil, nil, p.acquireErr
	}
	return p.ctrl, func() { p.releaseCalled = true }, nil
}

func (p *fakeAgentPool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	if p.headlessErr != nil {
		return nil, p.headlessErr
	}
	return p.headlessResult, nil
}

// recordingSink 记录全部 Emit 调用，供断言事件序列。
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingSink) Emit(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *recordingSink) hasKind(k EventKind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// ── 测试用例 ──────────────────────────────────────────────────────────────

func newTestOrchestrator(t *testing.T, persistence *fakePersistence, hooks *fakeHooks, slash *fakeSlash, compression *fakeCompression, pool *fakeAgentPool) Orchestrator {
	t.Helper()
	return New(Deps{
		Hooks:         hooks,
		SlashRouter:   slash,
		Compression:   compression,
		Persistence:   persistence,
		Prompt:        fakePrompt{},
		Registry:      fakeRegistry{},
		AgentPool:     pool,
		TranscriptDir: t.TempDir(),
		DataDir:       t.TempDir(),
	})
}

// TestRunTurn_Interactive_HappyPath 验证正常轮次：FSM 产出 token → sink 收到
// KindDelta → assistant 消息落盘 → complete 事件。
func TestRunTurn_Interactive_HappyPath(t *testing.T) {
	ctrl := newFakeAgentController()
	go func() {
		ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventToken, Content: "hello "}
		ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventToken, Content: "world"}
		ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventStatus, Content: "task_done"}
	}()

	persistence := newFakePersistence()
	pool := &fakeAgentPool{ctrl: ctrl}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, pool)
	sink := &recordingSink{}

	res, err := orc.RunTurn(context.Background(), Request{SessionID: "s1", Input: "hi", Channel: "web"}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if res.Reply != "hello world" {
		t.Errorf("Reply = %q, want %q", res.Reply, "hello world")
	}
	if !sink.hasKind(KindComplete) {
		t.Error("expected KindComplete event")
	}
	replies := persistence.savedAssistantReplies()
	if len(replies) != 1 || replies[0] != "hello world" {
		t.Errorf("saved assistant replies = %v, want [\"hello world\"]", replies)
	}
	if !pool.releaseCalled {
		t.Error("expected Acquire release() to be called")
	}
}

// TestRunTurn_Interactive_SlashShortCircuit 验证斜线命令短路：FSM 不应被触发
// （fakeAgentController 无事件产出也不会阻塞，因为 Dispatch 直接返回 Handled）。
func TestRunTurn_Interactive_SlashShortCircuit(t *testing.T) {
	ctrl := newFakeAgentController() // 不产出任何事件；若被误触发会导致测试挂起超时
	persistence := newFakePersistence()
	pool := &fakeAgentPool{ctrl: ctrl}
	slash := &fakeSlash{forceHandle: true, response: "命令已处理"}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, slash, &fakeCompression{}, pool)
	sink := &recordingSink{}

	res, err := orc.RunTurn(context.Background(), Request{SessionID: "s1", Input: "/help", Channel: "web"}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if !res.SlashHandled {
		t.Error("expected SlashHandled=true")
	}
	replies := persistence.savedAssistantReplies()
	if len(replies) != 1 || replies[0] != "命令已处理" {
		t.Errorf("saved assistant replies = %v", replies)
	}
	if !persistence.touched {
		t.Error("expected TouchSession called on slash path")
	}
}

// TestRunTurn_Interactive_HookBlocked 验证 message.before 拦截：不应调用
// AgentPool.Acquire（fakeAgentPool.acquireErr 若被触发会直接暴露）。
func TestRunTurn_Interactive_HookBlocked(t *testing.T) {
	persistence := newFakePersistence()
	pool := &fakeAgentPool{acquireErr: apperr.New(apperr.CodeInternal, "should not be called")}
	hooks := &fakeHooks{blockBefore: map[string]string{"message.before": "policy violation"}}
	orc := newTestOrchestrator(t, persistence, hooks, &fakeSlash{}, &fakeCompression{}, pool)
	sink := &recordingSink{}

	res, err := orc.RunTurn(context.Background(), Request{SessionID: "s1", Input: "bad input", Channel: "web"}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if !res.Aborted {
		t.Error("expected Aborted=true when hook blocks")
	}
	if !sink.hasKind(KindError) {
		t.Error("expected KindError event for hook_blocked")
	}
}

// TestRunTurn_Interactive_ClientAbort 验证客户端断连：ctx 提前取消时，已产出
// 的部分回复应落盘，且 Agent 被 Interrupt。
func TestRunTurn_Interactive_ClientAbort(t *testing.T) {
	ctrl := newFakeAgentController()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventToken, Content: "partial"}
		time.Sleep(20 * time.Millisecond)
		cancel() // 模拟客户端断连
	}()

	persistence := newFakePersistence()
	pool := &fakeAgentPool{ctrl: ctrl}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, pool)
	sink := &recordingSink{}

	res, err := orc.RunTurn(ctx, Request{SessionID: "s1", Input: "hi", Channel: "web"}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if !res.Aborted {
		t.Error("expected Aborted=true on client disconnect")
	}
	if !ctrl.interrupted {
		t.Error("expected Agent Interrupt() to be called on client disconnect (GD-13-002)")
	}
	replies := persistence.savedAssistantReplies()
	if len(replies) != 1 || replies[0] != "partial" {
		t.Errorf("expected partial reply saved, got %v", replies)
	}
}

// TestRunTurn_Headless_HappyPath 验证 Headless 路径：AcquireHeadless 输出
// 落盘（SystemPromptGuard 扫描职责在 AgentPool.AcquireHeadless 内部完成，见
// guard.go 顶部注释的 A-03 Step5 决策修正，本层不重复扫描），且 Complete
// 事件推送（bufferSink 用法）。
func TestRunTurn_Headless_HappyPath(t *testing.T) {
	persistence := newFakePersistence()
	pool := &fakeAgentPool{headlessResult: &types.AgentResult{Output: "automation reply", LatencyMs: 42}}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, pool)
	sink := NewBufferSink()

	res, err := orc.RunTurn(context.Background(), Request{
		SessionID: "wf-session-1",
		Input:     "run the workflow",
		Channel:   "workflow",
		Headless:  true,
		TitleHint: "My Workflow",
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if res.Reply != "automation reply" {
		t.Errorf("Reply = %q, want %q", res.Reply, "automation reply")
	}
	if sink.String() != "automation reply" {
		t.Errorf("BufferSink.String() = %q, want %q", sink.String(), "automation reply")
	}
	replies := persistence.savedAssistantReplies()
	if len(replies) != 1 || replies[0] != "automation reply" {
		t.Errorf("saved assistant replies = %v", replies)
	}
	if persistence.titled != "My Workflow" {
		t.Errorf("titled = %q, want TitleHint honored", persistence.titled)
	}
	if !persistence.touched {
		t.Error("expected TouchSession called on headless path (A-03 收敛价值锚点之一)")
	}
}

// TestRunTurn_Headless_HookBlocked 验证 Headless 路径同样受 message.before
// 拦截保护（收敛前 workflow/cron 两个入口完全没有这层防护）。
func TestRunTurn_Headless_HookBlocked(t *testing.T) {
	persistence := newFakePersistence()
	pool := &fakeAgentPool{headlessErr: apperr.New(apperr.CodeInternal, "should not be called")}
	hooks := &fakeHooks{blockBefore: map[string]string{"message.before": "blocked"}}
	orc := newTestOrchestrator(t, persistence, hooks, &fakeSlash{}, &fakeCompression{}, pool)
	sink := NewBufferSink()

	res, err := orc.RunTurn(context.Background(), Request{
		SessionID: "wf-session-2",
		Input:     "bad automation input",
		Channel:   "workflow",
		Headless:  true,
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if !res.Aborted {
		t.Error("expected Aborted=true when hook blocks headless turn")
	}
}
