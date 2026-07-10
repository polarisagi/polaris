package learning

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── mock 实现 ────────────────────────────────────────────────────────────────

type mockReflector struct {
	calls atomic.Int32
}

func (m *mockReflector) Reflect(_ context.Context, _, _ string, _ *TaskResult, _ []Step, _ int) (*Reflection, error) {
	m.calls.Add(1)
	return &Reflection{}, nil
}

type mockCurriculum struct {
	calls atomic.Int32
}

func (m *mockCurriculum) Generate(_ context.Context, _ float64) error {
	m.calls.Add(1)
	return nil
}

type mockRollout struct {
	calls     atomic.Int32
	lastStats RolloutStats
}

func (m *mockRollout) AdvanceGate(_ context.Context, _ string, stats RolloutStats) error {
	m.lastStats = stats
	m.calls.Add(1)
	return nil
}

type mockVersionStore struct{}

func (m *mockVersionStore) UpdateScore(ctx context.Context, id string, score float64) error {
	return nil
}
func (m *mockVersionStore) Activate(ctx context.Context, taskType, id string, baselineScore float64) error {
	return nil
}

// ── 辅助：构建带 buffer 通道的 Engine ────────────────────────────────────────

func newTestEngine(
	cfg *EngineConfig,
	r Reflector,
	c CurriculumGenerator,
	ro RolloutAdvancer,
) (*Engine, chan TaskCompleteEvent, chan VersionChangeEvent) {
	taskCh := make(chan TaskCompleteEvent, 8)
	verCh := make(chan VersionChangeEvent, 8)
	e := NewEngine(cfg, r, c, ro, taskCh, verCh)
	return e, taskCh, verCh
}

// ── TaskResult.IsUncontrollable ───────────────────────────────────────────────

func TestTaskResult_IsUncontrollable_True(t *testing.T) {
	r := &TaskResult{Success: false, FailureClass: FailureUncontrollable}
	if !r.IsUncontrollable() {
		t.Fatal("期望 true，得到 false")
	}
}

func TestTaskResult_IsUncontrollable_Success(t *testing.T) {
	r := &TaskResult{Success: true, FailureClass: FailureUncontrollable}
	if r.IsUncontrollable() {
		t.Fatal("Success=true 时期望 false，得到 true")
	}
}

func TestTaskResult_IsUncontrollable_LogicFailure(t *testing.T) {
	r := &TaskResult{Success: false, FailureClass: FailureLogic}
	if r.IsUncontrollable() {
		t.Fatal("FailureLogic 期望 false，得到 true")
	}
}

// ── Engine.Run() ─────────────────────────────────────────────────────────────

func TestEngine_Run_CancelCtx(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.MidLoopInterval = 100 * time.Millisecond

	e, _, _ := newTestEngine(cfg, &mockReflector{}, &mockCurriculum{}, &mockRollout{})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Start(ctx) }()

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("期望 context.Canceled，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() 未在 ctx 取消后返回")
	}
}

func TestEngine_Run_FailedTask_TriggersReflect(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.MidLoopInterval = 100 * time.Millisecond

	ref := &mockReflector{}
	e, taskCh, _ := newTestEngine(cfg, ref, &mockCurriculum{}, &mockRollout{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Start(ctx) }()

	taskCh <- TaskCompleteEvent{
		TaskID:  "t1",
		Success: false,
		Failure: FailureLogic,
	}

	// 等待异步 goroutine 完成
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ref.calls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if ref.calls.Load() < 1 {
		t.Fatalf("期望 Reflector.Reflect 至少被调用 1 次，实际 %d 次", ref.calls.Load())
	}
}

func TestEngine_Run_SuccessTask_NoReflect(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.MidLoopInterval = 100 * time.Millisecond

	ref := &mockReflector{}
	e, taskCh, _ := newTestEngine(cfg, ref, &mockCurriculum{}, &mockRollout{})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _ = e.Start(ctx) }()

	taskCh <- TaskCompleteEvent{
		TaskID:  "t2",
		Success: true,
	}

	// 等待 ctx 超时，确保 Engine 至少运行了 200ms
	<-ctx.Done()

	if ref.calls.Load() != 0 {
		t.Fatalf("成功任务不应触发 Reflect，实际调用 %d 次", ref.calls.Load())
	}
}

func TestEngine_Run_VersionEvent_TriggersRollout(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.MidLoopInterval = 100 * time.Millisecond

	ro := &mockRollout{}
	e, _, verCh := newTestEngine(cfg, &mockReflector{}, &mockCurriculum{}, ro)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Start(ctx) }()

	verCh <- VersionChangeEvent{
		CandidateVersion: "v0.2.0",
		Stats:            RolloutStats{ErrorRate: 0.01, BaselineErrorRate: 0.02},
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ro.calls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if ro.calls.Load() < 1 {
		t.Fatalf("期望 RolloutAdvancer.AdvanceGate 至少被调用 1 次，实际 %d 次", ro.calls.Load())
	}
}

func TestEngine_Run_MidLoop_TriggersCurriculum(t *testing.T) {
	cfg := &EngineConfig{
		MidLoopInterval:          30 * time.Millisecond,
		MaxConcurrentReflections: 3,
	}

	cur := &mockCurriculum{}
	e, _, _ := newTestEngine(cfg, &mockReflector{}, cur, &mockRollout{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Start(ctx) }()

	// 30ms ticker，400ms 内应触发至少 2 次（最坏情况 60ms << 400ms margin 充裕）
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cur.calls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if cur.calls.Load() < 2 {
		t.Fatalf("期望 curriculum.Generate 至少被调用 2 次，实际 %d 次", cur.calls.Load())
	}
}

// ── DefaultEngineConfig ───────────────────────────────────────────────────────

func TestEngine_DefaultConfig(t *testing.T) {
	cfg := DefaultEngineConfig()
	if cfg.MidLoopInterval != 2*time.Minute {
		t.Fatalf("MidLoopInterval 期望 2min，得到 %v", cfg.MidLoopInterval)
	}
	if cfg.MaxConcurrentReflections != 3 {
		t.Fatalf("MaxConcurrentReflections 期望 3，得到 %d", cfg.MaxConcurrentReflections)
	}
}

// TestEngine_handleEvalCompleted_SubmitsToStaging 验证 2026-07-10 后的新行为：
// Eval 达标后提交 Staging 进入 Gate 2(Shadow)，不再直接调用 rollout.AdvanceGate
// 或 versionStore.Activate——真正的激活推迟到 ShadowExecutor 确认之后
// （见 optimizer.SQLiteRolloutStore.ConfirmShadow，ADR-0029 §K）。
func TestEngine_handleEvalCompleted_SubmitsToStaging(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.BaselinePassRate = 0.8

	ro := &mockRollout{}
	e, _, _ := newTestEngine(cfg, &mockReflector{}, &mockCurriculum{}, ro)
	e.SetVersionStore(&mockVersionStore{})

	var submitted *optimizer.AgentVersionSnapshot
	var recordCalls atomic.Int32
	var recordedPassRate float64
	sp := &mockStagingPipeline{
		submitFn: func(_ context.Context, snap *optimizer.AgentVersionSnapshot) error {
			submitted = snap
			return nil
		},
		recordFn: func(_ context.Context, _ string, passRate, _ float64) error {
			recordedPassRate = passRate
			recordCalls.Add(1)
			return nil
		},
	}
	e.SetStagingPipeline(sp)

	ctx := context.Background()
	e.handleEvalCompleted(ctx, types.EvalCompletedPayload{
		Suite:            "validation",
		CandidateID:      "cand-safe",
		PassRate:         0.9,
		SafetyViolations: 1,
		P95LatencyMs:     150,
		BaselineP95Ms:    100,
	})

	if submitted == nil {
		t.Fatal("expected SubmitCandidate to be called")
	}
	if submitted.Version != "cand-safe" {
		t.Errorf("expected Version=cand-safe, got %v", submitted.Version)
	}
	if submitted.TaskType != "validation" {
		t.Errorf("expected TaskType=validation, got %v", submitted.TaskType)
	}
	if submitted.BaselineScore != 0.8 {
		t.Errorf("expected BaselineScore=0.8, got %v", submitted.BaselineScore)
	}
	if recordCalls.Load() == 0 {
		t.Fatal("expected RecordEvalScore to be called")
	}
	if recordedPassRate != 0.9 {
		t.Errorf("expected recorded passRate=0.9, got %v", recordedPassRate)
	}

	// 新设计下 Eval 通过不再同步触发 AdvanceGate（该调用推迟到 ConfirmShadow 之后
	// 由 versionEvents 通路或 ConfirmShadow 内部处理），此处应保持未调用。
	if ro.calls.Load() != 0 {
		t.Errorf("expected AdvanceGate NOT to be called synchronously from handleEvalCompleted, got %d calls", ro.calls.Load())
	}
}
