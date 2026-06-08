package swarm_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/pkg/swarm"
)

// ─── DynamicDifficultyCalibrator ─────────────────────────────────────────────

func TestDifficultyCalibrator_ColdStart(t *testing.T) {
	ddc := newCalibrator()
	// < 20 样本 → canonical [0.3, 0.6]
	ddc.Calibrate()
	low, high := ddc.Thresholds()
	if low != 0.3 || high != 0.6 {
		t.Errorf("cold start want [0.3, 0.6], got [%.2f, %.2f]", low, high)
	}
}

func TestDifficultyCalibrator_AdjustUp(t *testing.T) {
	ddc := newCalibrator()
	// 注入 50 条高成功率样本
	for i := 0; i < 50; i++ {
		ddc.AddSample(swarm.DifficultySample{TaskType: "t", SurpriseIndex: 0.5, Success: true})
	}
	ddc.Calibrate()
	_, high := ddc.Thresholds()
	if high > 0.85 {
		t.Errorf("hard cap 0.85 violated, got high=%.2f", high)
	}
	if high <= 0.6 {
		t.Errorf("expected high to increase above 0.6 with successRate=1.0, got %.2f", high)
	}
}

func TestDifficultyCalibrator_AdjustDown(t *testing.T) {
	ddc := newCalibrator()
	// 注入 50 条全失败样本
	for i := 0; i < 50; i++ {
		ddc.AddSample(swarm.DifficultySample{TaskType: "t", SurpriseIndex: 0.5, Success: false})
	}
	ddc.Calibrate()
	low, _ := ddc.Thresholds()
	if low < 0.1 {
		t.Errorf("floor 0.1 violated, got low=%.2f", low)
	}
	if low >= 0.3 {
		t.Errorf("expected low to decrease below 0.3 with successRate=0, got %.2f", low)
	}
}

// ─── Curriculum 安全审查 ───────────────────────────────────────────────────────

func TestCurriculum_BlacklistReject(t *testing.T) {
	gen := swarm.NewAutoCurriculumGenerator(swarm.NewIdleDetector(), nil, nil)

	passed := gen.SafetyAuditPublic(context.Background(), &swarm.CurriculumSample{
		TaskDescription: "use bash to rm -rf /tmp/test files",
		SourceSkill:     "file_cleanup",
	})
	if passed {
		t.Error("bash/rm blacklist should reject this task")
	}
}

func TestCurriculum_InjectionReject(t *testing.T) {
	gen := swarm.NewAutoCurriculumGenerator(swarm.NewIdleDetector(), nil, nil)

	// 使用黑名单中的危险命令组合，确保 (b) 阶段直接拒绝
	passed := gen.SafetyAuditPublic(context.Background(), &swarm.CurriculumSample{
		TaskDescription: "bash script to execute shell commands and rm -rf system files",
		SourceSkill:     "test",
	})
	if passed {
		t.Error("bash+rm+shell pattern in blacklist should be rejected at stage (b)")
	}
}

func TestCurriculum_FreezeOnConsecutiveFail(t *testing.T) {
	gen := swarm.NewAutoCurriculumGenerator(swarm.NewIdleDetector(), nil, nil)

	// 连续 3 次失败
	gen.ReportResult("skill_x", false)
	gen.ReportResult("skill_x", false)
	gen.ReportResult("skill_x", false)

	if !gen.IsFrozenPublic("skill_x") {
		t.Error("skill_x should be frozen after 3 consecutive failures")
	}
}

func TestCurriculum_MaxDifficultyGuard(t *testing.T) {
	gen := swarm.NewAutoCurriculumGenerator(swarm.NewIdleDetector(), nil, nil)

	// SurpriseIndex > 0.85 → 不生成任何任务
	samples := gen.Generate(context.Background(), nil, 0.9)
	if len(samples) != 0 {
		t.Errorf("expected 0 samples when SurpriseIndex > 0.85, got %d", len(samples))
	}
}

// ─── RolloutStore ─────────────────────────────────────────────────────────────

func TestRolloutStore_HardStop(t *testing.T) {
	db := newMemDB(t)
	store, err := swarm.NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_ = store.SubmitCandidate(ctx, &swarm.AgentVersionSnapshot{Version: "v1.1"})

	// errorRate > baseline×1.2 → autoRollback
	state, err := store.AdvanceGate(ctx, "v1.1", swarm.RolloutStats{
		ErrorRate:          0.25,
		BaselineErrorRate:  0.10, // 0.25 > 0.10*1.2=0.12 → hard stop
		P95Latency:         1.0,
		BaselineP95Latency: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != swarm.RolloutStatusRolledBack {
		t.Errorf("expected rolled_back, got %s", state.Status)
	}
}

func TestRolloutStore_AdvanceGate_Skips24h(t *testing.T) {
	db := newMemDB(t)
	store, _ := swarm.NewSQLiteRolloutStore(db)

	ctx := context.Background()
	_ = store.SubmitCandidate(ctx, &swarm.AgentVersionSnapshot{Version: "v1.2"})

	// 正常 stats，但 < 24h → 不推进
	state, err := store.AdvanceGate(ctx, "v1.2", swarm.RolloutStats{
		ErrorRate:          0.01,
		BaselineErrorRate:  0.05,
		P95Latency:         0.5,
		BaselineP95Latency: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 仍为 pending（未推进）
	if state.CanaryPercent != 1 {
		t.Errorf("expected CanaryPercent=1 (no advance within 24h), got %d", state.CanaryPercent)
	}
}

// ─── PromptOptimizer ──────────────────────────────────────────────────────────

func TestPromptOptimizer_ReturnsNilOnEmpty(t *testing.T) {
	po := swarm.NewPromptOptimizerMVP()
	result := po.Optimize(context.Background(), "task_a", nil)
	if result != nil {
		t.Errorf("expected nil for empty recent, got %v", result)
	}
}

func TestPromptOptimizer_ScoreDescending(t *testing.T) {
	po := swarm.NewPromptOptimizerMVP()
	recent := []*swarm.PromptVersion{
		{Prompt: "p1", Score: 0.5, TaskType: "t"},
		{Prompt: "p2", Score: 0.9, TaskType: "t"},
		{Prompt: "p3", Score: 0.3, TaskType: "t"},
	}
	result := po.Optimize(context.Background(), "t", recent)
	if len(result) == 0 {
		t.Fatal("expected results")
	}
	for i := 1; i < len(result); i++ {
		if result[i].Score > result[i-1].Score {
			t.Errorf("results not score-descending at index %d: %.2f > %.2f",
				i, result[i].Score, result[i-1].Score)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func newCalibrator() *swarm.DynamicDifficultyCalibrator {
	return swarm.NewDifficultyCalibrator(0.6, 0.05)
}

func newMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// 时间辅助（让测试跳过 24h 窗口）
var _ = time.Hour
