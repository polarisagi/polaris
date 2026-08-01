package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/pkg/types"
)

// handoffResumeToolExecutor 是本文件专用的工具执行桩：按工具名记录调用次数，
// 供端到端测试断言"哪些节点真正被调度执行"（而非依赖某个全局共享 mock）。
type handoffResumeToolExecutor struct {
	mu    sync.Mutex
	calls map[string]int
}

func newHandoffResumeToolExecutor() *handoffResumeToolExecutor {
	return &handoffResumeToolExecutor{calls: make(map[string]int)}
}

func (e *handoffResumeToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
}

func (e *handoffResumeToolExecutor) ExecuteWithTaint(_ context.Context, name string, _ []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	e.mu.Lock()
	e.calls[name]++
	e.mu.Unlock()
	return &types.ToolResult{Success: true, Output: []byte(`{"tool":"` + name + `"}`)}, nil
}

func (e *handoffResumeToolExecutor) callCount(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[name]
}

func simpleValidDAGModel() *fsm.DAGModel {
	return &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
}

// TestBuildHandoffResumeSnapshot_ProducesValidJSON 验证挂起时快照落盘：
// resume_ctx_json 非空且可反序列化，字段与 sCtx 一致（阶段04 A-02 测试1）。
func TestBuildHandoffResumeSnapshot_ProducesValidJSON(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.sCtx.DAGModel = simpleValidDAGModel()
	a.sCtx.ExecuteResult = []byte(`{"n1":"done"}`)
	a.sCtx.CompletedNodeIDs = []string{"n1"}
	a.sCtx.GlobalTaintLevel = types.TaintMedium
	a.sCtx.HandoffTaskID = "handoff-child-x"
	a.sCtx.NamespaceID = "ns-1"

	jsonStr, err := a.buildHandoffResumeSnapshot()
	if err != nil {
		t.Fatalf("buildHandoffResumeSnapshot failed: %v", err)
	}
	if jsonStr == "" {
		t.Fatal("expected non-empty resume_ctx_json")
	}

	var snap HandoffResumeContext
	if uerr := json.Unmarshal([]byte(jsonStr), &snap); uerr != nil {
		t.Fatalf("resume_ctx_json is not valid JSON: %v", uerr)
	}
	if snap.SchemaVersion != HandoffResumeContextVersion {
		t.Errorf("expected schema version %d, got %d", HandoffResumeContextVersion, snap.SchemaVersion)
	}
	if snap.DAGModel == nil || len(snap.DAGModel.Nodes) != 1 {
		t.Fatalf("expected DAGModel with 1 node roundtripped, got %+v", snap.DAGModel)
	}
	if snap.HandoffNodeID != "handoff-child-x" {
		t.Errorf("expected HandoffNodeID roundtripped, got %q", snap.HandoffNodeID)
	}
	if snap.GlobalTaintLevel != types.TaintMedium {
		t.Errorf("expected GlobalTaintLevel roundtripped, got %v", snap.GlobalTaintLevel)
	}
}

// TestResumeAwaitingHandoff_RestoresValidSnapshot 验证新建 Agent +
// ResumeAwaitingHandoff(id, json) → sCtx.DAGModel 非 nil、restored == true
// （阶段04 A-02 测试2）。
func TestResumeAwaitingHandoff_RestoresValidSnapshot(t *testing.T) {
	source := newTestHandoffAgent(t)
	source.sCtx.DAGModel = simpleValidDAGModel()
	source.sCtx.CompletedNodeIDs = []string{}
	jsonStr, err := source.buildHandoffResumeSnapshot()
	if err != nil {
		t.Fatalf("buildHandoffResumeSnapshot failed: %v", err)
	}

	fresh := NewAgentWithDefaults("resume-target")
	fresh.InjectPolicyGate(&allowAllGate{})

	restored := fresh.ResumeAwaitingHandoff("handoff-child-x", jsonStr)
	if !restored {
		t.Fatal("expected restored == true for a valid snapshot")
	}
	if fresh.sCtx.DAGModel == nil {
		t.Fatal("expected sCtx.DAGModel to be non-nil after restore")
	}
	if fresh.sm.Current() != types.AgentStateAwaitAgent {
		t.Errorf("expected FSM to be forced into S_AWAIT_AGENT, got %v", fresh.sm.Current())
	}
}

// TestResumeAwaitingHandoff_RejectsWhenPolicyGateDenies 验证快照中的 DAG
// 未能通过重校验（PolicyGate 拒绝，模拟"篡改后含非法/被禁工具"场景）时
// restored == false 且 DAGModel 保持 nil（阶段04 A-02 测试3：安全边界——
// 反序列化输入必须重新过校验，不能因"来自自家表"而被信任）。
func TestResumeAwaitingHandoff_RejectsWhenPolicyGateDenies(t *testing.T) {
	source := newTestHandoffAgent(t)
	source.sCtx.DAGModel = simpleValidDAGModel()
	jsonStr, err := source.buildHandoffResumeSnapshot()
	if err != nil {
		t.Fatalf("buildHandoffResumeSnapshot failed: %v", err)
	}

	fresh := NewAgentWithDefaults("resume-target-denied")
	fresh.InjectPolicyGate(&denyAllGate{})

	restored := fresh.ResumeAwaitingHandoff("handoff-child-x", jsonStr)
	if restored {
		t.Fatal("expected restored == false when re-validation is rejected")
	}
	if fresh.sCtx.DAGModel != nil {
		t.Errorf("expected sCtx.DAGModel to remain nil when re-validation is rejected, got %+v", fresh.sCtx.DAGModel)
	}
}

// TestResumeAwaitingHandoff_RejectsUnknownSchemaVersion 验证 SchemaVersion
// 不匹配时 restored == false 且无 panic（阶段04 A-02 测试4）。
func TestResumeAwaitingHandoff_RejectsUnknownSchemaVersion(t *testing.T) {
	tampered := `{"v":999,"dag_model":{"Nodes":[{"ID":"n1","ToolName":"read_file"}]}}`

	fresh := NewAgentWithDefaults("resume-target-badversion")
	fresh.InjectPolicyGate(&allowAllGate{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ResumeAwaitingHandoff panicked on unknown schema version: %v", r)
		}
	}()
	restored := fresh.ResumeAwaitingHandoff("handoff-child-x", tampered)
	if restored {
		t.Fatal("expected restored == false for unknown schema version")
	}
}

// TestResumeAwaitingHandoff_TaintOnlyUp 验证污点 only-up：快照中的
// GlobalTaintLevel 更低时不得覆盖当前已持有的更高污点等级（阶段04 A-02
// 测试5，ADR-0007 防降级攻击）。
func TestResumeAwaitingHandoff_TaintOnlyUp(t *testing.T) {
	source := newTestHandoffAgent(t)
	source.sCtx.DAGModel = simpleValidDAGModel()
	source.sCtx.GlobalTaintLevel = types.TaintNone
	jsonStr, err := source.buildHandoffResumeSnapshot()
	if err != nil {
		t.Fatalf("buildHandoffResumeSnapshot failed: %v", err)
	}

	fresh := NewAgentWithDefaults("resume-target-taint")
	fresh.InjectPolicyGate(&allowAllGate{})
	fresh.sCtx.GlobalTaintLevel = types.TaintHigh

	restored := fresh.ResumeAwaitingHandoff("handoff-child-x", jsonStr)
	if !restored {
		t.Fatal("expected restored == true")
	}
	if fresh.sCtx.GlobalTaintLevel != types.TaintHigh {
		t.Errorf("expected GlobalTaintLevel to remain TaintHigh (only-up), got %v", fresh.sCtx.GlobalTaintLevel)
	}
}

// TestBuildHandoffResumeSnapshot_OversizedSkipsPersist 验证快照超出
// handoffSnapshotMaxBytes 时不落盘（返回 error，调用方据此写入空字符串，
// 恢复走降级路径），阶段04 A-02 测试7。
func TestBuildHandoffResumeSnapshot_OversizedSkipsPersist(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.sCtx.DAGModel = simpleValidDAGModel()
	// 构造一个远超 256KB 上限的 ExecuteResult。
	a.sCtx.ExecuteResult = []byte(strings.Repeat("x", handoffSnapshotMaxBytes+1024))

	jsonStr, err := a.buildHandoffResumeSnapshot()
	if err == nil {
		t.Fatal("expected error when snapshot exceeds handoffSnapshotMaxBytes")
	}
	if jsonStr != "" {
		t.Errorf("expected empty resume_ctx_json on oversized snapshot, got %d bytes", len(jsonStr))
	}

	// 空字符串走旧的"仅消除死锁"降级路径：restored 必为 false。
	fresh := NewAgentWithDefaults("resume-target-oversized")
	fresh.InjectPolicyGate(&allowAllGate{})
	restored := fresh.ResumeAwaitingHandoff("handoff-child-x", jsonStr)
	if restored {
		t.Fatal("expected restored == false when resumeCtxJSON is empty (degraded path)")
	}
}

// TestHandoffCrashRecovery_ResumesDownstreamDAGNode 端到端验证：3 节点 DAG
// （n1 → n2[transfer_to_agent] → n3），模拟进程崩溃重启——n1/n2 已完成的
// 事实通过快照的 CompletedNodeIDs 回填给全新 Agent 实例，n3 在恢复后的
// runExecuteDAG 中被真正执行（修复前 DAGModel 从未恢复，会命中 nil 快速
// 路径直接终态，n3 永不执行）（阶段04 A-02 测试6）。
func TestHandoffCrashRecovery_ResumesDownstreamDAGNode(t *testing.T) {
	const childID = "handoff-child-e2e"

	dagModel := &fsm.DAGModel{
		Nodes: []dag.ExecNode{
			{ID: "n1", ToolName: "noop1"},
			{ID: "n2", ToolName: "transfer_to_agent", DependsOn: []string{"n1"},
				Args: []byte(`{"target_agent_role":"librarian","context_summary":"ctx"}`)},
			{ID: "n3", ToolName: "noop3", DependsOn: []string{"n2"}},
		},
		Edges: []dag.ExecEdge{{From: "n1", To: "n2"}, {From: "n2", To: "n3"}},
	}

	// 构造"崩溃前"的快照：n1 已完成，n2 是挂起的 handoff 节点本身
	// （CompletedNodeIDs 不含 n2——它由 executeTransferToAgent 的恢复检查
	// 分支处理，不经 PreCompletedNodes 跳过），n3 尚未执行。
	snapSource := newTestHandoffAgent(t)
	snapSource.sCtx.DAGModel = dagModel
	snapSource.sCtx.CompletedNodeIDs = []string{"n1"}
	snapSource.sCtx.ExecuteResult = []byte(`{"n1":"prior result"}`)
	jsonStr, err := snapSource.buildHandoffResumeSnapshot()
	if err != nil {
		t.Fatalf("buildHandoffResumeSnapshot failed: %v", err)
	}

	// "崩溃重启"：全新 Agent 实例，通过 Reconciler 路径恢复。
	fresh := NewAgentWithDefaults("resume-target-e2e")
	fresh.InjectPolicyGate(&allowAllGate{})
	toolExec := newHandoffResumeToolExecutor()
	fresh.InjectToolExecutor(toolExec)

	// handoffPoster：子任务已终态 Done，使 n2 的"恢复检查"分支直接返回结果，
	// 不重新挂起。
	poster := &fakeHandoffPoster{tasks: map[string]*types.TaskSnapshot{
		childID: {ID: childID, Status: types.TaskDone, Result: []byte("delegated result")},
	}}
	fresh.InjectHandoffPoster(poster)

	restored := fresh.ResumeAwaitingHandoff(childID, jsonStr)
	if !restored {
		t.Fatal("expected restored == true")
	}

	if err := fresh.runExecuteDAG(context.Background()); err != nil {
		t.Fatalf("runExecuteDAG failed: %v", err)
	}

	if got := toolExec.callCount("noop1"); got != 0 {
		t.Errorf("expected n1 (noop1) to be skipped via PreCompletedNodes, got %d calls", got)
	}
	if got := toolExec.callCount("noop3"); got != 1 {
		t.Errorf("expected n3 (noop3) to be executed exactly once after resume, got %d calls", got)
	}
	if !strings.Contains(string(fresh.sCtx.ExecuteResult), "prior result") {
		t.Errorf("expected merged ExecuteResult to retain pre-crash prior result, got %s", fresh.sCtx.ExecuteResult)
	}
	if !strings.Contains(string(fresh.sCtx.ExecuteResult), "noop3") {
		t.Errorf("expected merged ExecuteResult to include post-resume n3 output, got %s", fresh.sCtx.ExecuteResult)
	}
}
