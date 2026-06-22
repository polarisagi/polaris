package learning

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	extskill "github.com/polarisagi/polaris/internal/extension/skill"
)

// mockTrajectoryCompiler 编译成功，直接返回固定 CompileResult。
type mockTrajectoryCompiler struct {
	compileFn func(ctx context.Context, req *extskill.CompileRequest) (*extskill.CompileResult, error)
}

func (m *mockTrajectoryCompiler) Compile(ctx context.Context, req *extskill.CompileRequest) (*extskill.CompileResult, error) {
	return m.compileFn(ctx, req)
}

// mockStagingPipeline 记录 SubmitCandidate 调用。
type mockStagingPipeline struct {
	submitFn func(ctx context.Context, snap *optimizer.AgentVersionSnapshot) error
}

func (m *mockStagingPipeline) SubmitCandidate(ctx context.Context, snap *optimizer.AgentVersionSnapshot) error {
	return m.submitFn(ctx, snap)
}
func (m *mockStagingPipeline) RecordEvalScore(_ context.Context, _ string, _, _ float64) error {
	return nil
}
func (m *mockStagingPipeline) ConfirmShadow(_ context.Context, _ string) error { return nil }
func (m *mockStagingPipeline) AdvanceGate(_ context.Context, _ string, _ optimizer.RolloutStats) (*optimizer.RolloutState, error) {
	return nil, nil
}
func (m *mockStagingPipeline) Rollback(_ context.Context, _ string, _ string) error { return nil }
func (m *mockStagingPipeline) GetState(_ context.Context, _ string) (*optimizer.RolloutState, error) {
	return nil, nil
}

// TestLogicCollapseMonitor_TriggerCollapse_SubmitStaging 验证编译成功后提交 Staging 候选。
func TestLogicCollapseMonitor_TriggerCollapse_SubmitStaging(t *testing.T) {
	var submitted *optimizer.AgentVersionSnapshot
	mockPipeline := &mockStagingPipeline{
		submitFn: func(_ context.Context, snap *optimizer.AgentVersionSnapshot) error {
			submitted = snap
			return nil
		},
	}

	mockCompiler := &mockTrajectoryCompiler{
		compileFn: func(_ context.Context, _ *extskill.CompileRequest) (*extskill.CompileResult, error) {
			return &extskill.CompileResult{
				ScriptHash:  "abc123",
				RiskLevel:   "low",
				SandboxTier: 3,
			}, nil
		},
	}

	monitor := NewLogicCollapseMonitor(mockCompiler, nil, nil, nil, nil, t.TempDir())
	monitor.WithStagingPipeline(mockPipeline)

	traj := &extskill.CollapseTrajectory{
		SkillID:      "test_skill",
		SuccessCount: 60,
		RiskLevel:    "low",
	}
	monitor.triggerCollapse(context.Background(), traj, 0.5)

	if submitted == nil {
		t.Fatal("expected SubmitCandidate to be called, but it was not")
	}
	if submitted.SkillSnapshotID != "test_skill" {
		t.Errorf("unexpected SkillSnapshotID: %s", submitted.SkillSnapshotID)
	}
	if !strings.HasPrefix(submitted.Version, "skill-test_skill-") {
		t.Errorf("unexpected Version format: %s", submitted.Version)
	}
}

// TestLogicCollapseMonitor_TriggerCollapse_NilPipeline_NoError 验证 nil Pipeline 时不 panic。
func TestLogicCollapseMonitor_TriggerCollapse_NilPipeline_NoError(t *testing.T) {
	mockCompiler := &mockTrajectoryCompiler{
		compileFn: func(_ context.Context, _ *extskill.CompileRequest) (*extskill.CompileResult, error) {
			return &extskill.CompileResult{
				ScriptHash:  "abc",
				RiskLevel:   "low",
				SandboxTier: 1,
			}, nil
		},
	}
	monitor := NewLogicCollapseMonitor(mockCompiler, nil, nil, nil, nil, t.TempDir())
	// stagingPipeline 未注入（nil）

	traj := &extskill.CollapseTrajectory{
		SkillID:      "skill_x",
		SuccessCount: 60,
		RiskLevel:    "low",
	}
	// 不应 panic
	monitor.triggerCollapse(context.Background(), traj, 0.3)
}
