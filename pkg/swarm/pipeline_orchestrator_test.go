package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── stub Blackboard for pipeline tests ──────────────────────────────────────

type stubBlackboard struct {
	tasks    map[string]*protocol.TaskEntry
	outcomes map[string]taskOutcome // taskID → preset outcome
}

type taskOutcome struct {
	status protocol.TaskStatus
	result []byte
}

func newStubBlackboard(outcomes map[string]taskOutcome) *stubBlackboard {
	return &stubBlackboard{
		tasks:    make(map[string]*protocol.TaskEntry),
		outcomes: outcomes,
	}
}

func (s *stubBlackboard) PostTask(_ context.Context, task *protocol.TaskEntry) error {
	s.tasks[task.ID] = task
	return nil
}

func (s *stubBlackboard) PostBatch(_ context.Context, tasks []*protocol.TaskEntry) error {
	for _, t := range tasks {
		s.tasks[t.ID] = t
	}
	return nil
}

func (s *stubBlackboard) PeekTask(_ context.Context, id string) (*protocol.TaskSnapshot, error) {
	oc, ok := s.outcomes[id]
	if !ok {
		return &protocol.TaskSnapshot{ID: id, Status: protocol.TaskPending}, nil
	}
	return &protocol.TaskSnapshot{ID: id, Status: oc.status, Result: oc.result}, nil
}

// Unused interface methods — stub implementations.
func (s *stubBlackboard) ClaimTask(_ context.Context, _, _ string) (bool, error) { return true, nil }
func (s *stubBlackboard) StartExecution(_ context.Context, _, _ string) error    { return nil }
func (s *stubBlackboard) CompleteTask(_ context.Context, _, _ string, _ []byte) error {
	return nil
}
func (s *stubBlackboard) FailTask(_ context.Context, _, _ string, _ []byte) error { return nil }
func (s *stubBlackboard) RenewLease(_ context.Context, _, _ string) error         { return nil }
func (s *stubBlackboard) SuspendForHITL(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (s *stubBlackboard) ResumeFromHITL(_ context.Context, _, _ string, _ bool) error { return nil }
func (s *stubBlackboard) BeginCompensation(_ context.Context, _, _ string) error      { return nil }
func (s *stubBlackboard) EndCompensation(_ context.Context, _, _ string) error        { return nil }
func (s *stubBlackboard) SideEffectPreCheck(_ context.Context, _, _ string, _ int32) error {
	return nil
}
func (s *stubBlackboard) Subscribe(_ context.Context) (<-chan protocol.BlackboardEvent, error) {
	ch := make(chan protocol.BlackboardEvent)
	return ch, nil
}

// ─── tests ───────────────────────────────────────────────────────────────────

func TestPipelineOrchestrator_SingleStage_Pass(t *testing.T) {
	result, _ := json.Marshal(map[string]string{"output": "research complete"})
	bb := newStubBlackboard(map[string]taskOutcome{
		"pipe-test-research-0": {status: protocol.TaskDone, result: result},
	})

	po := NewPipelineOrchestrator(bb, 10*time.Millisecond)
	desc := protocol.PipelineDescriptor{
		ID:   "pipe-test",
		Goal: "research topic X",
		Stages: []protocol.PipelineStageSpec{
			{Name: "research", Capability: "research", TaskType: "research", Priority: 1},
		},
	}

	vr, err := po.Run(context.Background(), desc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vr.Verdict != protocol.VerdictPass {
		t.Errorf("expected VerdictPass, got %v", vr.Verdict)
	}
}

func TestPipelineOrchestrator_MultiStage_ContextPayloadPropagation(t *testing.T) {
	researchOut, _ := json.Marshal(map[string]string{"libraries": "go-kit, prometheus"})
	planOut, _ := json.Marshal(map[string]string{"plan": "step 1, step 2"})
	execOut, _ := json.Marshal(map[string]string{"summary": "implemented"})
	verifyOut, _ := json.Marshal(protocol.VerificationResult{
		Verdict: protocol.VerdictPass,
		Summary: "goal achieved",
	})

	bb := newStubBlackboard(map[string]taskOutcome{
		"pipe-mp-research-0": {status: protocol.TaskDone, result: researchOut},
		"pipe-mp-plan-0":     {status: protocol.TaskDone, result: planOut},
		"pipe-mp-execute-0":  {status: protocol.TaskDone, result: execOut},
		"pipe-mp-verify":     {status: protocol.TaskDone, result: verifyOut},
	})

	po := NewPipelineOrchestrator(bb, 10*time.Millisecond)
	desc := protocol.PipelineDescriptor{
		ID:   "pipe-mp",
		Goal: "implement observability feature",
		Stages: []protocol.PipelineStageSpec{
			{Name: "research", Capability: "research", TaskType: "research"},
			{Name: "plan", Capability: "plan", TaskType: "plan"},
			{Name: "execute", Capability: "execute", TaskType: "execute"},
		},
		VerificationPolicy: &protocol.VerificationPolicy{
			Adversarial: true,
			BlockOnFail: true,
		},
	}

	vr, err := po.Run(context.Background(), desc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vr.Verdict != protocol.VerdictPass {
		t.Errorf("expected VerdictPass, got %v", vr.Verdict)
	}

	// 验证 context_payload 传播：plan 阶段任务应携带 research 的产出
	planTask, ok := bb.tasks["pipe-mp-plan-0"]
	if !ok {
		t.Fatal("plan task not found in blackboard")
	}
	if len(planTask.ContextPayload) == 0 {
		t.Error("plan task should have ContextPayload from research stage")
	}
	var planCtx map[string]string
	if err := json.Unmarshal(planTask.ContextPayload, &planCtx); err != nil {
		t.Fatalf("ContextPayload is not valid JSON: %v", err)
	}
	if planCtx["libraries"] != "go-kit, prometheus" {
		t.Errorf("plan ContextPayload should carry research output, got %v", planCtx)
	}

	// 验证 PipelineID 和 PipelineStage 字段正确填充
	if planTask.PipelineID != "pipe-mp" {
		t.Errorf("expected PipelineID=pipe-mp, got %s", planTask.PipelineID)
	}
	if planTask.PipelineStage != "plan" {
		t.Errorf("expected PipelineStage=plan, got %s", planTask.PipelineStage)
	}
}

func TestPipelineOrchestrator_VerificationBlocker(t *testing.T) {
	execOut, _ := json.Marshal(map[string]string{"summary": "partial implementation"})
	blockerOut, _ := json.Marshal(protocol.VerificationResult{
		Verdict: protocol.VerdictBlocker,
		Summary: "core feature missing",
		Findings: []protocol.VerificationFinding{
			{Verdict: protocol.VerdictBlocker, Description: "handler not wired"},
		},
	})

	bb := newStubBlackboard(map[string]taskOutcome{
		"pipe-blk-execute-0": {status: protocol.TaskDone, result: execOut},
		"pipe-blk-verify":    {status: protocol.TaskDone, result: blockerOut},
	})

	po := NewPipelineOrchestrator(bb, 10*time.Millisecond)
	desc := protocol.PipelineDescriptor{
		ID:   "pipe-blk",
		Goal: "add handler",
		Stages: []protocol.PipelineStageSpec{
			{Name: "execute", Capability: "execute", TaskType: "execute"},
		},
		VerificationPolicy: &protocol.VerificationPolicy{
			Adversarial: true,
			BlockOnFail: true,
		},
	}

	vr, err := po.Run(context.Background(), desc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vr.Verdict != protocol.VerdictBlocker {
		t.Errorf("expected VerdictBlocker, got %v", vr.Verdict)
	}
}

func TestPipelineOrchestrator_EmptyStages_Error(t *testing.T) {
	bb := newStubBlackboard(nil)
	po := NewPipelineOrchestrator(bb, 10*time.Millisecond)
	_, err := po.Run(context.Background(), protocol.PipelineDescriptor{ID: "pipe-empty"})
	if err == nil {
		t.Error("expected error for empty pipeline")
	}
}

func TestPipelineOrchestrator_StageFailed_Error(t *testing.T) {
	bb := newStubBlackboard(map[string]taskOutcome{
		"pipe-fail-research-0": {status: protocol.TaskFailed, result: nil},
	})

	po := NewPipelineOrchestrator(bb, 10*time.Millisecond)
	desc := protocol.PipelineDescriptor{
		ID:   "pipe-fail",
		Goal: "will fail",
		Stages: []protocol.PipelineStageSpec{
			{Name: "research", Capability: "research", TaskType: "research"},
		},
		MaxRetries: 0,
	}

	_, err := po.Run(context.Background(), desc)
	if err == nil {
		t.Error("expected error when stage fails")
	}
}

func TestPipelineOrchestrator_ContextCancellation(t *testing.T) {
	// 黑板返回 Pending，永不完成 → context 取消应触发超时错误
	bb := newStubBlackboard(nil) // outcomes 为空 → 总返回 TaskPending
	po := NewPipelineOrchestrator(bb, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := po.Run(ctx, protocol.PipelineDescriptor{
		ID:   "pipe-cancel",
		Goal: "will be cancelled",
		Stages: []protocol.PipelineStageSpec{
			{Name: "research", Capability: "research", TaskType: "research"},
		},
	})
	if err == nil {
		t.Error("expected context timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// 错误链中应有 DeadlineExceeded 或包裹的 context.Canceled
		t.Logf("error (acceptable): %v", err)
	}
}
