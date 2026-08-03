package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeHandoffPoster 是 HandoffPoster 的纯内存测试替身。
type fakeHandoffPoster struct {
	mu         sync.Mutex
	tasks      map[string]*types.TaskSnapshot
	lastPosted *types.TaskEntry

	// 阶段04 A-01：事件订阅相关测试挂钩。
	subscribeErr   error                      // 非 nil 时 Subscribe 返回该错误（走轮询降级路径）
	subscribeCh    chan types.BlackboardEvent // 非 nil 时 Subscribe 返回该通道，供测试注入事件
	subscribeCalls int
	peekCalls      int
}

func (f *fakeHandoffPoster) PostTask(_ context.Context, task *types.TaskEntry) error {
	f.mu.Lock()
	f.lastPosted = task
	f.tasks[task.ID] = &types.TaskSnapshot{ID: task.ID, Status: types.TaskPending, Namespace: task.Namespace, Type: task.Type}
	f.mu.Unlock()
	return nil
}

func (f *fakeHandoffPoster) SubscribeTaskEvents(taskID string) (<-chan types.AgentStreamEvent, func()) {
	return nil, func() {}
}

func (f *fakeHandoffPoster) PeekTask(_ context.Context, taskID string) (*types.TaskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peekCalls++
	snap, ok := f.tasks[taskID]
	if !ok || snap == nil {
		return nil, nil
	}
	// 返回副本而非内部 map 值的指针：调用方（watcher/reconciler 的轮询
	// goroutine）会在释放锁之后读取返回值字段，若直接返回内部指针，
	// 测试里通过 poster.mu 保护的"写"和调用方无锁的"读"会形成数据竞争
	// （-race 可复现，2026-07-29 AwaitingHandoffReconciler 测试引入时发现）。
	cp := *snap
	return &cp, nil
}

// Subscribe 是 HandoffPoster.Subscribe 的测试替身（阶段04 A-01）：
// 返回 f.subscribeErr（若非 nil）或懒初始化的 f.subscribeCh，供测试用例
// 通过 poster.subscribeCh <- ev 注入事件。
func (f *fakeHandoffPoster) Subscribe(_ context.Context) (<-chan types.BlackboardEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribeCalls++
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	if f.subscribeCh == nil {
		f.subscribeCh = make(chan types.BlackboardEvent, 8)
	}
	return f.subscribeCh, nil
}

func (f *fakeHandoffPoster) peekCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peekCalls
}

func newTestHandoffAgent(t *testing.T) *Agent {
	t.Helper()
	a := NewAgent("handoff-test-agent", nil, nil)
	a.sCtx.SessionID = "session-1"
	return a
}

func TestExecuteTransferToAgent_Success(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.sCtx.NamespaceID = "ns-shared"
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := a.executeTransferToAgent(ctx, "librarian", "please summarize X", types.TaintMedium)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !res.Suspended {
		t.Fatalf("expected Suspended=true, got false")
	}
	if string(res.Output) != "Agent suspended waiting for handoff task completion." {
		t.Errorf("expected suspended message, got %s", res.Output)
	}
	if poster.lastPosted == nil {
		t.Fatal("expected a task to have been posted")
	}
	if poster.lastPosted.Namespace != "ns-shared" {
		t.Errorf("expected task namespace to reuse current NamespaceID, got %q", poster.lastPosted.Namespace)
	}
	if poster.lastPosted.Type != "agent_handoff:librarian" {
		t.Errorf("expected task type 'agent_handoff:librarian', got %q", poster.lastPosted.Type)
	}
}

func TestExecuteTransferToAgent_NamespaceFallsBackToSessionID(t *testing.T) {
	a := newTestHandoffAgent(t)
	// NamespaceID 留空，验证退化为 SessionID。
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.executeTransferToAgent(ctx, "librarian", "x", types.TaintLow); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if poster.lastPosted.Namespace != a.sCtx.SessionID {
		t.Errorf("expected namespace to fall back to SessionID %q, got %q", a.sCtx.SessionID, poster.lastPosted.Namespace)
	}
}

func TestExecuteTransferToAgent_MissingRole(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	_, err := a.executeTransferToAgent(context.Background(), "", "x", types.TaintLow)
	if err == nil {
		t.Fatal("expected error when target_agent_role is empty")
	}
}

func TestExecuteTransferToAgent_NilPosterFailsClosed(t *testing.T) {
	a := newTestHandoffAgent(t)
	// 未注入 HandoffPoster：必须 fail-closed 返回错误，而非 panic。
	_, err := a.executeTransferToAgent(context.Background(), "librarian", "x", types.TaintLow)
	if err == nil {
		t.Fatal("expected fail-closed error when handoffPoster is nil")
	}
}

// TestWatchHandoffCompletion_FiresResumeOnDone 回归测试 GD-1 修复：
// watcher 检测到子任务 Done 后，必须投递 TriggerAgentHandoffDone 唤醒 FSM，
// 而不是（修复前的）投递 TriggerSuspend 导致 Dispatch 返回
// "no transition from S_AWAIT_AGENT with trigger TriggerSuspend" 硬错误。
//
// 阶段04 A-01：改为事件驱动后，本用例验证主路径——poster.Subscribe 推送
// 匹配 TaskID 的 task_completed 事件应唤醒 Agent，且 PeekTask 调用次数
// ≤ 1（初始"补一次 Peek"覆盖丢事件窗口，之后不再轮询，证明不再走
// GD-13-007 修复前的 1s ticker 轮询）。
func TestWatchHandoffCompletion_FiresResumeOnDone(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-1"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}
	poster.mu.Unlock()

	a.watchHandoffCompletion(childID)

	// 等待 watcher 完成订阅 + 初始 Peek，再推送匹配事件。
	time.Sleep(50 * time.Millisecond)
	poster.mu.Lock()
	ch := poster.subscribeCh
	poster.mu.Unlock()
	if ch == nil {
		t.Fatal("expected watcher to have called Subscribe by now")
	}
	ch <- types.BlackboardEvent{Type: "task_completed", TaskID: childID}

	select {
	case trigger := <-a.intent:
		if trigger != types.TriggerAgentHandoffDone {
			t.Fatalf("expected TriggerAgentHandoffDone, got %v", trigger)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watcher to fire TriggerAgentHandoffDone")
	}

	if got := poster.peekCallCount(); got > 1 {
		t.Errorf("expected PeekTask called at most once (initial lost-event check only), got %d — 说明退化回轮询", got)
	}
}

// TestWatchHandoffCompletion_IgnoresMismatchedTaskID 验证 Subscribe 是全局
// 广播：推送不匹配 TaskID 的事件不应唤醒当前 watcher（阶段04 A-01 硬性约束2）。
func TestWatchHandoffCompletion_IgnoresMismatchedTaskID(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-2"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}
	poster.mu.Unlock()

	a.watchHandoffCompletion(childID)

	time.Sleep(50 * time.Millisecond)
	poster.mu.Lock()
	ch := poster.subscribeCh
	poster.mu.Unlock()
	if ch == nil {
		t.Fatal("expected watcher to have called Subscribe by now")
	}
	ch <- types.BlackboardEvent{Type: "task_completed", TaskID: "some-other-task"}

	select {
	case trigger := <-a.intent:
		t.Fatalf("expected no wake from mismatched TaskID event, got trigger %v", trigger)
	case <-time.After(300 * time.Millisecond):
		// 期望超时——未被误唤醒。
	}
}

// TestWatchHandoffCompletion_SubscribeErrorFallsBackToPolling 验证 Subscribe
// 失败时不静默放弃，而是退化为轮询兜底且仍能唤醒 + 记录 Warn 日志
// （阶段04 A-01：订阅失败不能导致父任务永久挂起）。
func TestWatchHandoffCompletion_SubscribeErrorFallsBackToPolling(t *testing.T) {
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(origLogger)

	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{
		tasks:        make(map[string]*types.TaskSnapshot),
		subscribeErr: errors.New("subscribe unavailable"),
	}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-3"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}
	poster.mu.Unlock()

	a.watchHandoffCompletion(childID)

	// 轮询降级路径间隔 1s，短暂等待后翻转为 Done。
	time.Sleep(50 * time.Millisecond)
	poster.mu.Lock()
	poster.tasks[childID].Status = types.TaskDone
	poster.mu.Unlock()

	select {
	case trigger := <-a.intent:
		if trigger != types.TriggerAgentHandoffDone {
			t.Fatalf("expected TriggerAgentHandoffDone, got %v", trigger)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for polling fallback to fire TriggerAgentHandoffDone")
	}

	if !strings.Contains(logBuf.String(), "handoff subscribe failed, falling back to polling") {
		t.Errorf("expected Warn log about polling fallback, got log output: %s", logBuf.String())
	}
}

// TestWatchHandoffCompletion_WakesViaInitialPeekWhenAlreadyTerminal 验证
// "先订阅、再补一次 Peek" 覆盖 PostTask 到 Subscribe 之间子任务已终态的
// 丢事件窗口（阶段04 A-01 核心竞态修复锚点）。
func TestWatchHandoffCompletion_WakesViaInitialPeekWhenAlreadyTerminal(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-4"
	// 子任务在 watcher 启动之前就已终态（模拟极短子任务在 Subscribe 生效前
	// 就完成的竞态）。
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskDone}
	poster.mu.Unlock()

	a.watchHandoffCompletion(childID)

	select {
	case trigger := <-a.intent:
		if trigger != types.TriggerAgentHandoffDone {
			t.Fatalf("expected TriggerAgentHandoffDone, got %v", trigger)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial peek to fire TriggerAgentHandoffDone")
	}
}

// ─── ADR-0084: MCP A2A 深度/污点/PII 相关测试 ──────────────────────────────

// TestExecuteTransferToAgent_SpawnDepthIncrementsFromSCtx 验证委派链深度
// 正确从 a.sCtx.SpawnDepth 递增写入 entry.SpawnDepth（此前恒为 0，深度上限
// 从未真正生效，见 ADR-0084 决策3）。
func TestExecuteTransferToAgent_SpawnDepthIncrementsFromSCtx(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.sCtx.SpawnDepth = 2
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	if _, err := a.executeTransferToAgent(context.Background(), "librarian", "x", types.TaintLow); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if poster.lastPosted.SpawnDepth != 3 {
		t.Errorf("expected posted SpawnDepth=3 (parent 2 + 1), got %d", poster.lastPosted.SpawnDepth)
	}
}

// TestExecuteTransferToAgent_MCPTargetForcesTaintHighOnResume 验证 ADR-0084 A1：
// mcp: 前缀目标的委派结果恢复时强制 TaintHigh，即使发起方自身污点更低。
func TestExecuteTransferToAgent_MCPTargetForcesTaintHighOnResume(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-mcp-child"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskDone, Result: []byte("external result")}
	poster.mu.Unlock()
	a.sCtx.HandoffTaskID = childID

	res, err := a.executeTransferToAgent(context.Background(), "mcp:linear/researcher", "x", types.TaintLow)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.TaintLevel != types.TaintHigh {
		t.Errorf("expected TaintHigh forced for mcp: target, got %v", res.TaintLevel)
	}
}

// TestExecuteTransferToAgent_NonMCPTargetDoesNotForceHighTaint 验证本地角色
// 委派（非 mcp: 前缀）不受 A1 强制高污点规则影响，保持既有行为。
func TestExecuteTransferToAgent_NonMCPTargetDoesNotForceHighTaint(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-local-child"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskDone, Result: []byte("local result")}
	poster.mu.Unlock()
	a.sCtx.HandoffTaskID = childID

	res, err := a.executeTransferToAgent(context.Background(), "librarian", "x", types.TaintLow)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.TaintLevel != types.TaintLow {
		t.Errorf("expected TaintLevel unchanged (TaintLow) for non-mcp target, got %v", res.TaintLevel)
	}
}

// TestExecuteTransferToAgent_MCPTargetRedactsPII 验证 ADR-0084 A6：mcp: 前缀
// 目标投递前对 context_summary 做 PII 静态脱敏。
func TestExecuteTransferToAgent_MCPTargetRedactsPII(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.Security.PIIDetector = guard.NewPIIDetector()
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const summary = "contact user at leaked@example.com for details"
	if _, err := a.executeTransferToAgent(context.Background(), "mcp:linear/researcher", summary, types.TaintLow); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	posted := string(poster.lastPosted.Intent)
	if strings.Contains(posted, "leaked@example.com") {
		t.Errorf("expected email to be redacted before mcp: egress, got intent: %s", posted)
	}
	if !strings.Contains(posted, "REDACTED") {
		t.Errorf("expected [REDACTED:...] marker in posted intent, got: %s", posted)
	}
}

// TestExecuteTransferToAgent_NonMCPTargetSkipsPIIRedaction 验证本地角色委派
// 不受 A6 出境脱敏规则影响（该规则只约束离开信任边界的 mcp: 委派）。
func TestExecuteTransferToAgent_NonMCPTargetSkipsPIIRedaction(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.Security.PIIDetector = guard.NewPIIDetector()
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const summary = "contact user at kept@example.com for details"
	if _, err := a.executeTransferToAgent(context.Background(), "librarian", summary, types.TaintLow); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if string(poster.lastPosted.Intent) != summary {
		t.Errorf("expected non-mcp target intent to pass through unmodified, got: %s", poster.lastPosted.Intent)
	}
}

// TestExecuteTransferToAgent_MCPTargetNilPIIDetectorSkipsRedaction 验证
// PIIDetector 未注入时降级放行（不阻断主流程），与既有 headless 路径 PII
// 降级策略一致。
func TestExecuteTransferToAgent_MCPTargetNilPIIDetectorSkipsRedaction(t *testing.T) {
	a := newTestHandoffAgent(t)
	// a.Security.PIIDetector 保持 nil。
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const summary = "contact user at plain@example.com for details"
	if _, err := a.executeTransferToAgent(context.Background(), "mcp:linear/researcher", summary, types.TaintLow); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if string(poster.lastPosted.Intent) != summary {
		t.Errorf("expected passthrough when PIIDetector is nil, got: %s", poster.lastPosted.Intent)
	}
}

// TestWatchHandoffCompletion_CtxCancelExitsWithoutLeak 验证 Agent ctx 取消后
// watcher goroutine 退出（不残留），Subscribe 通道后续写入不会阻塞/panic。
func TestWatchHandoffCompletion_CtxCancelExitsWithoutLeak(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-5"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}
	poster.mu.Unlock()

	a.watchHandoffCompletion(childID)
	time.Sleep(50 * time.Millisecond)

	a.cancel() // Agent 生命周期 ctx 取消

	select {
	case trigger := <-a.intent:
		t.Fatalf("expected no wake after ctx cancel, got trigger %v", trigger)
	case <-time.After(300 * time.Millisecond):
		// 期望超时——goroutine 已随 ctx 取消退出，未产生任何唤醒。
	}
}
