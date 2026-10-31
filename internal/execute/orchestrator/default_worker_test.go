package orchestrator

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockAgentPool 记录 AcquireHeadless 收到的 query，并按配置返回成功/失败，
// 用于验证 DefaultTaskWorker 是否正确把 TaskEntry.Intent 转交给 headless 推理，
// 以及正确处理成功/失败两条路径的 Blackboard 写回。
type mockAgentPool struct {
	mu          sync.Mutex
	queries     []string
	replyOutput string
	replyTaint  types.TaintLevel
	failWith    error
}

func (p *mockAgentPool) Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error) {
	return nil, func() {}, nil
}

func (p *mockAgentPool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	p.mu.Lock()
	p.queries = append(p.queries, intent.Query)
	p.mu.Unlock()
	if p.failWith != nil {
		return nil, p.failWith
	}
	return &types.AgentResult{Output: p.replyOutput, TaintLevel: p.replyTaint}, nil
}

func (p *mockAgentPool) queriesForTest() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.queries))
	copy(out, p.queries)
	return out
}

// TestDefaultTaskWorker_ClaimsAndExecutesOrphanedTask 验证核心修复行为：
// 之前无人认领的 "agent_query" 类型任务，现在会被 DefaultTaskWorker 认领，
// 其 Intent 原样作为 headless query 执行，成功后写回 CompleteTask。
func TestDefaultTaskWorker_ClaimsAndExecutesOrphanedTask(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-aq-1"] = &types.TaskEntry{
		ID:     "task-aq-1",
		Type:   "agent_query",
		Status: types.TaskPending,
		Intent: []byte("what is the weather"),
	}

	pool := &mockAgentPool{replyOutput: "sunny"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-aq-1"}
	time.Sleep(50 * time.Millisecond)

	bb.mu.Lock()
	entry := bb.tasks["task-aq-1"]
	bb.mu.Unlock()

	if entry.Status != types.TaskDone {
		t.Fatalf("expected task to be claimed and completed, got status %v", entry.Status)
	}
	if entry.ClaimedBy != defaultTaskWorkerAgentID {
		t.Fatalf("expected task claimed by %q, got %q", defaultTaskWorkerAgentID, entry.ClaimedBy)
	}
	queries := pool.queriesForTest()
	if len(queries) != 1 || queries[0] != "what is the weather" {
		t.Fatalf("expected AcquireHeadless called once with the task's Intent, got %v", queries)
	}
}

func TestDefaultTaskWorkerPersistsHeadlessResultTaintThroughSQLite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := newSchemaBackedDB(t)
	defer db.Close()
	bb := NewSQLiteBlackboard(db)
	pool := &mockAgentPool{replyOutput: "reviewed result", replyTaint: types.TaintUserReviewed}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")
	go func() { _ = worker.RunLoop(ctx) }()
	time.Sleep(20 * time.Millisecond) // allow the worker to subscribe before posting

	if err := bb.PostTask(ctx, &types.TaskEntry{
		ID: "task-taint-4", Type: "agent_query", Intent: []byte("offline fixture"), IntentTaint: types.TaintHigh,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := bb.PeekTask(context.Background(), "task-taint-4")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Status == types.TaskDone {
			if string(snapshot.Result) != "reviewed result" {
				t.Fatalf("unexpected durable result %q", snapshot.Result)
			}
			if snapshot.ResultTaint != types.TaintUserReviewed {
				t.Fatalf("DefaultTaskWorker persisted taint=%v want TaintUserReviewed(4)", snapshot.ResultTaint)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot, err := bb.PeekTask(context.Background(), "task-taint-4")
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("worker did not persist task completion: status=%v", snapshot.Status)
}

// TestDefaultTaskWorker_SkipsExcludedType 验证 excludeTypes 生效：属于专用
// Worker（如 workflow_step_worker.go）能力类型的任务不会被本 Worker 抢占，
// 避免把结构化 JSON intent 当纯文本传给 LLM。
func TestDefaultTaskWorker_SkipsExcludedType(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-wf-1"] = &types.TaskEntry{
		ID:     "task-wf-1",
		Type:   "workflow_step",
		Status: types.TaskPending,
		Intent: []byte(`{"state_graph_node_id":"n1"}`),
	}

	pool := &mockAgentPool{replyOutput: "should not be called"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-wf-1"}
	time.Sleep(50 * time.Millisecond)

	bb.mu.Lock()
	entry := bb.tasks["task-wf-1"]
	bb.mu.Unlock()

	if entry.Status != types.TaskPending {
		t.Fatalf("expected excluded-type task to remain untouched (Pending), got %v", entry.Status)
	}
	if len(pool.queriesForTest()) != 0 {
		t.Fatalf("expected AcquireHeadless never called for excluded type, got %v", pool.queriesForTest())
	}
}

// TestDefaultTaskWorker_HeadlessFailureFailsTask 验证 AcquireHeadless 报错时
// 走 FailTask 而非静默吞掉，避免任务永久卡在 claimed 状态。
func TestDefaultTaskWorker_HeadlessFailureFailsTask(t *testing.T) {
	bb := &mockFailTrackingBlackboard{
		mockBlackboard: mockBlackboard{
			tasks:  make(map[string]*types.TaskEntry),
			events: make(chan types.BlackboardEvent, 10),
		},
	}
	bb.tasks["task-fail-1"] = &types.TaskEntry{
		ID:     "task-fail-1",
		Type:   "provider_recovered",
		Status: types.TaskPending,
		Intent: []byte("resume this"),
	}

	pool := &mockAgentPool{failWith: context.DeadlineExceeded}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-fail-1"}
	time.Sleep(50 * time.Millisecond)

	if !bb.failCalledForTest() {
		t.Fatal("expected FailTask to be called after AcquireHeadless returns an error")
	}
}

// TestDefaultTaskWorker_SkipsMCPA2APrefixExcludedType 验证 ADR-0084 引入的
// 前缀匹配排除：Type 带 "agent_handoff:mcp:" 前缀（非精确等于该前缀本身）的
// 任务同样被排除，不被当作纯文本 headless 查询执行。
func TestDefaultTaskWorker_SkipsMCPA2APrefixExcludedType(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-mcp-1"] = &types.TaskEntry{
		ID:     "task-mcp-1",
		Type:   "agent_handoff:mcp:linear/researcher",
		Status: types.TaskPending,
		Intent: []byte("summarize X"),
	}

	pool := &mockAgentPool{replyOutput: "should not be called"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step", MCPA2AHandoffPrefix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-mcp-1"}
	time.Sleep(50 * time.Millisecond)

	bb.mu.Lock()
	entry := bb.tasks["task-mcp-1"]
	bb.mu.Unlock()

	if entry.Status != types.TaskPending {
		t.Fatalf("expected mcp: prefixed task to remain untouched (Pending), got %v", entry.Status)
	}
	if len(pool.queriesForTest()) != 0 {
		t.Fatalf("expected AcquireHeadless never called for mcp: prefixed task, got %v", pool.queriesForTest())
	}
}

// TestDefaultTaskWorker_PassesSpawnDepthToHeadlessOptions 验证 ADR-0084
// SpawnDepth 透传：DefaultTaskWorker 派生 headless 任务时携带
// snap.SpawnDepth，供 AcquireHeadless 注入新建 Agent 的 sCtx。
func TestDefaultTaskWorker_PassesSpawnDepthToHeadlessOptions(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-depth-1"] = &types.TaskEntry{
		ID:         "task-depth-1",
		Type:       "agent_query",
		Status:     types.TaskPending,
		Intent:     []byte("x"),
		SpawnDepth: 2,
	}

	pool := &spawnDepthCapturingPool{replyOutput: "ok"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-depth-1"}
	time.Sleep(50 * time.Millisecond)

	if got := pool.lastSpawnDepthForTest(); got != 2 {
		t.Errorf("expected SpawnDepth 2 passed to AcquireHeadless, got %d", got)
	}
}

// TestDefaultTaskWorker_PassesNamespaceToHeadlessOptions 验证 GD-14-001
// Namespace 透传（2026-08-02 补齐）：DefaultTaskWorker 派生 headless 任务时
// 携带 snap.Namespace，供 AcquireHeadless 注入 AgentController.SetMemoryNamespace，
// 使本地 agent_handoff:<role> 委派任务与发起方共享记忆检索范围（此前恒为空串，
// 见 99-new-findings.md 阶段05 P-03 续 发现 / ADR-0084"已知限制"）。
func TestDefaultTaskWorker_PassesNamespaceToHeadlessOptions(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-ns-1"] = &types.TaskEntry{
		ID:        "task-ns-1",
		Type:      "agent_handoff:librarian",
		Status:    types.TaskPending,
		Intent:    []byte("x"),
		Namespace: "swarm-ns-shared",
	}

	pool := &spawnDepthCapturingPool{replyOutput: "ok"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-ns-1"}
	time.Sleep(50 * time.Millisecond)

	if got := pool.lastNamespaceForTest(); got != "swarm-ns-shared" {
		t.Errorf("expected Namespace %q passed to AcquireHeadless, got %q", "swarm-ns-shared", got)
	}
}

// spawnDepthCapturingPool 记录 AcquireHeadless 收到的 HeadlessOptions.SpawnDepth/
// Namespace（2026-08-02 补充 Namespace 捕获，见
// TestDefaultTaskWorker_PassesNamespaceToHeadlessOptions）。
type spawnDepthCapturingPool struct {
	mu             sync.Mutex
	replyOutput    string
	lastSpawnDepth int
	lastNamespace  string
	lastProfile    *types.AgentProfileSpec
}

func (p *spawnDepthCapturingPool) Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error) {
	return nil, func() {}, nil
}

func (p *spawnDepthCapturingPool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	opt := &types.HeadlessOptions{}
	for _, o := range opts {
		o(opt)
	}
	p.mu.Lock()
	p.lastSpawnDepth = opt.SpawnDepth
	p.lastNamespace = opt.Namespace
	p.lastProfile = opt.Profile
	p.mu.Unlock()
	return &types.AgentResult{Output: p.replyOutput}, nil
}

func (p *spawnDepthCapturingPool) lastSpawnDepthForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSpawnDepth
}

func (p *spawnDepthCapturingPool) lastNamespaceForTest() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastNamespace
}

func (p *spawnDepthCapturingPool) lastProfileForTest() *types.AgentProfileSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastProfile
}

// mockFailTrackingBlackboard 包一层 mockBlackboard，记录 FailTask 是否被调用
// （基类的 FailTask 是无操作 stub，无法从外部观测调用与否）。
type mockFailTrackingBlackboard struct {
	mockBlackboard
	mu         sync.Mutex
	failCalled bool
}

func (b *mockFailTrackingBlackboard) FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error {
	b.mu.Lock()
	b.failCalled = true
	b.mu.Unlock()
	return nil
}

func (b *mockFailTrackingBlackboard) failCalledForTest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failCalled
}

type mapProfileResolver map[string]*types.AgentProfileSpec

func (m mapProfileResolver) ResolveAgentProfile(_ context.Context, name string) (*types.AgentProfileSpec, error) {
	if p, ok := m[name]; ok {
		return p, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, "unknown agent "+name)
}

// TestDefaultTaskWorker_ResolvesAgentProfile ADR-0103 决策三：agent_handoff:<name> 以角色执行。
func TestDefaultTaskWorker_ResolvesAgentProfile(t *testing.T) {
	bb := &mockBlackboard{tasks: make(map[string]*types.TaskEntry), events: make(chan types.BlackboardEvent, 10)}
	bb.tasks["task-p-1"] = &types.TaskEntry{ID: "task-p-1", Type: "agent_handoff:review:security", Status: types.TaskPending, Intent: []byte("x")}
	pool := &spawnDepthCapturingPool{replyOutput: "ok"}
	spec := &types.AgentProfileSpec{Name: "review:security"}
	worker := NewDefaultTaskWorker(bb, pool).WithSubagents(NewSubagentRunner(pool, mapProfileResolver{"review:security": spec}, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()
	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-p-1"}
	time.Sleep(50 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.lastProfile != spec {
		t.Fatalf("profile not passed to AcquireHeadless: %+v", pool.lastProfile)
	}
}

func TestSubagentRunner_ResolveRules(t *testing.T) {
	r := NewSubagentRunner(nil, mapProfileResolver{}, nil)
	for _, name := range []string{"", "general-purpose"} {
		if p, err := r.resolve(context.Background(), name); p != nil || err != nil {
			t.Errorf("%q must run as a general agent: %v %v", name, p, err)
		}
	}
	if _, err := r.resolve(context.Background(), "ghost"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("unknown agent must fail the task, got %v", err)
	}
}

// TestDefaultTaskWorker_PeerTaskRunsWithPeerProfile ADR-0111：对端委派任务必须以注入的
// peer 角色（只读、禁止再委派）执行。
func TestDefaultTaskWorker_PeerTaskRunsWithPeerProfile(t *testing.T) {
	bb := &mockBlackboard{tasks: make(map[string]*types.TaskEntry), events: make(chan types.BlackboardEvent, 10)}
	bb.tasks["peer-1"] = &types.TaskEntry{ID: "peer-1", Type: PeerDelegationTaskType, Status: types.TaskPending,
		Intent: []byte("x"), Namespace: "peer:abc"}
	pool := &spawnDepthCapturingPool{replyOutput: "ok"}
	spec := &types.AgentProfileSpec{Name: "peer-delegation", ReadOnly: true}
	worker := NewDefaultTaskWorker(bb, pool).WithPeerProfile(func(*types.TaskSnapshot) *types.AgentProfileSpec { return spec })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()
	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "peer-1"}
	time.Sleep(50 * time.Millisecond)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.lastProfile != spec || pool.lastNamespace != "peer:abc" {
		t.Fatalf("peer task must run with the peer profile in its isolated namespace, got %+v ns=%q", pool.lastProfile, pool.lastNamespace)
	}
}

// TestDefaultTaskWorker_PeerTaskFailsClosedWithoutProfile 负向用例：未注入 peer 角色时，
// 对端任务不得以本机用户的完整权限执行，而是直接失败。
func TestDefaultTaskWorker_PeerTaskFailsClosedWithoutProfile(t *testing.T) {
	bb := &mockFailTrackingBlackboard{mockBlackboard: mockBlackboard{tasks: make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10)}}
	bb.tasks["peer-2"] = &types.TaskEntry{ID: "peer-2", Type: PeerDelegationTaskType, Status: types.TaskPending, Intent: []byte("rm -rf")}
	pool := &mockAgentPool{replyOutput: "must not run"}
	worker := NewDefaultTaskWorker(bb, pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()
	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "peer-2"}
	time.Sleep(50 * time.Millisecond)

	if len(pool.queriesForTest()) != 0 || !bb.failCalledForTest() {
		t.Fatalf("peer task without profile must fail closed, queries=%v failCalled=%v", pool.queriesForTest(), bb.failCalledForTest())
	}
}

// listingBlackboard 在 mockBlackboard 上补 PendingTaskLister。
type listingBlackboard struct{ *mockBlackboard }

func (b listingBlackboard) ListPendingTaskIDs(context.Context, int) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id, e := range b.tasks {
		if e.Status == types.TaskPending {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// TestDefaultTaskWorker_PicksUpBacklogOnStart 重启前投递、未被认领的任务不会再有 task_posted 事件；
// Worker 启动时必须补扫，否则任务一直饿到 reaper 的 60 分钟超时（对端节点委派来的任务就此卡在 working）。
func TestDefaultTaskWorker_PicksUpBacklogOnStart(t *testing.T) {
	inner := &mockBlackboard{tasks: make(map[string]*types.TaskEntry), events: make(chan types.BlackboardEvent, 10)}
	inner.tasks["task-backlog"] = &types.TaskEntry{ID: "task-backlog", Type: PeerDelegationTaskType, Status: types.TaskPending,
		Intent: []byte("list files")}
	inner.tasks["task-excluded"] = &types.TaskEntry{ID: "task-excluded", Type: "workflow_step", Status: types.TaskPending,
		Intent: []byte("{}")}
	pool := &mockAgentPool{replyOutput: "ok"}
	worker := NewDefaultTaskWorker(listingBlackboard{inner}, pool, "workflow_step").
		WithPeerProfile(func(*types.TaskSnapshot) *types.AgentProfileSpec {
			return &types.AgentProfileSpec{Name: "peer", ReadOnly: true}
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		inner.mu.Lock()
		st := inner.tasks["task-backlog"].Status
		ex := inner.tasks["task-excluded"].Status
		inner.mu.Unlock()
		if st == types.TaskDone {
			if ex != types.TaskPending {
				t.Fatalf("backlog sweep must still respect excluded types, got %v", ex)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending task posted before startup must be picked up without an event, status %v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSQLiteBlackboard_ListPendingTaskIDs(t *testing.T) {
	ctx := context.Background()
	bb := NewSQLiteBlackboard(newSchemaBackedDB(t))
	for _, id := range []string{"p1", "p2", "c1"} {
		if err := bb.PostTask(ctx, &types.TaskEntry{ID: id, Type: "agent_query", Intent: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := bb.ClaimTask(ctx, "c1", "w"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	ids, err := bb.ListPendingTaskIDs(ctx, 0)
	if err != nil || len(ids) != 2 {
		t.Fatalf("only unclaimed tasks are pending, got %v %v", ids, err)
	}
	var _ PendingTaskLister = bb
}

// TestDefaultTaskWorker_SpawnTaskSpec_StructuredIntent 验证 DefaultTaskWorker 正确解析 SpawnTaskSpec 结构化 Intent。
func TestDefaultTaskWorker_SpawnTaskSpec_StructuredIntent(t *testing.T) {
	bb := &mockBlackboard{tasks: make(map[string]*types.TaskEntry), events: make(chan types.BlackboardEvent, 10)}
	spec := types.SpawnTaskSpec{
		Prompt:   "analyze logs",
		Role:     "reviewer",
		Model:    "openai/o3-mini",
		Thinking: "high",
		ReadOnly: true,
		Tools:    []string{"read_file", "grep"},
	}
	specBytes, _ := json.Marshal(spec)
	bb.tasks["task-spec-1"] = &types.TaskEntry{
		ID:     "task-spec-1",
		Type:   "agent_handoff:reviewer",
		Status: types.TaskPending,
		Intent: specBytes,
	}

	pool := &spawnDepthCapturingPool{replyOutput: "done"}
	worker := NewDefaultTaskWorker(bb, pool).WithSubagents(NewSubagentRunner(pool, mapProfileResolver{"reviewer": {Name: "reviewer"}}, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()
	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-spec-1"}

	deadline := time.Now().Add(2 * time.Second)
	for {
		bb.mu.Lock()
		st := bb.tasks["task-spec-1"].Status
		bb.mu.Unlock()
		if st == types.TaskDone {
			prof := pool.lastProfileForTest()
			if prof == nil {
				t.Fatal("expected profile to be injected")
			}
			if prof.Model != "openai/o3-mini" {
				t.Errorf("expected model openai/o3-mini, got %s", prof.Model)
			}
			if prof.Thinking != "high" {
				t.Errorf("expected thinking high, got %s", prof.Thinking)
			}
			if !prof.ReadOnly {
				t.Error("expected ReadOnly=true")
			}
			if !prof.AllowDelegation {
				t.Error("local subagents should default to AllowDelegation=true")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not finish in time, status: %v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDefaultTaskWorker_PeerDelegation_DisallowsDelegation 验证跨节点任务严格禁止再委派（ADR-0111 / ADR-0115）。
func TestDefaultTaskWorker_PeerDelegation_DisallowsDelegation(t *testing.T) {
	bb := &mockBlackboard{tasks: make(map[string]*types.TaskEntry), events: make(chan types.BlackboardEvent, 10)}
	bb.tasks["peer-task-1"] = &types.TaskEntry{
		ID:     "peer-task-1",
		Type:   PeerDelegationTaskType,
		Status: types.TaskPending,
		Intent: []byte("remote prompt"),
	}

	pool := &spawnDepthCapturingPool{replyOutput: "peer done"}
	peerProf := func(snap *types.TaskSnapshot) *types.AgentProfileSpec {
		return &types.AgentProfileSpec{
			Name:            "peer:remote",
			ReadOnly:        true,
			AllowDelegation: false, // 严格禁止再委派
		}
	}
	worker := NewDefaultTaskWorker(bb, pool).WithPeerProfile(peerProf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()
	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "peer-task-1"}

	deadline := time.Now().Add(2 * time.Second)
	for {
		bb.mu.Lock()
		st := bb.tasks["peer-task-1"].Status
		bb.mu.Unlock()
		if st == types.TaskDone {
			prof := pool.lastProfileForTest()
			if prof == nil {
				t.Fatal("expected peer profile to be injected")
			}
			if prof.AllowDelegation {
				t.Fatal("peer delegation must strictly disallow further delegation (AllowDelegation=false)")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer task did not finish in time, status: %v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
