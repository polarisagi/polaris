package curriculum

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	_ "modernc.org/sqlite"
)

// 2026-07-14（ADR-0051）：DynamicDifficultyCalibrator/CoEvolutionCoordinator/
// AutoConfigOptimizer 整体删除（calibrator.go）——全仓生产零调用点，是与
// internal/prompt/optimizer/memf.go 同名但独立、从未被采纳的平行实现（该包
// 真正使用的动态难度校准是 memf.go 自己的 DynamicDifficultyCalibrator）。原
// TestDifficultyCalibrator_ColdStart/_AdjustUp/_AdjustDown 随之删除。

// ─── Curriculum 安全审查 ───────────────────────────────────────────────────────

func TestCurriculum_BlacklistReject(t *testing.T) {
	gen := NewAutoCurriculumGenerator(NewIdleDetector(), nil, nil)

	passed := gen.SafetyAuditPublic(context.Background(), &CurriculumSample{
		TaskDescription: "use bash to rm -rf /tmp/test files",
		SourceSkill:     "file_cleanup",
	})
	if passed {
		t.Error("bash/rm blacklist should reject this task")
	}
}

func TestCurriculum_InjectionReject(t *testing.T) {
	gen := NewAutoCurriculumGenerator(NewIdleDetector(), nil, nil)

	// 使用黑名单中的危险命令组合，确保 (b) 阶段直接拒绝
	passed := gen.SafetyAuditPublic(context.Background(), &CurriculumSample{
		TaskDescription: "bash script to execute shell commands and rm -rf system files",
		SourceSkill:     "test",
	})
	if passed {
		t.Error("bash+rm+shell pattern in blacklist should be rejected at stage (b)")
	}
}

func TestCurriculum_FreezeOnConsecutiveFail(t *testing.T) {
	gen := NewAutoCurriculumGenerator(NewIdleDetector(), nil, nil)

	// 连续 3 次失败
	gen.ReportResult("skill_x", false)
	gen.ReportResult("skill_x", false)
	gen.ReportResult("skill_x", false)

	if !gen.IsFrozenPublic("skill_x") {
		t.Error("skill_x should be frozen after 3 consecutive failures")
	}
}

func TestCurriculum_MaxDifficultyGuard(t *testing.T) {
	gen := NewAutoCurriculumGenerator(NewIdleDetector(), nil, nil)

	// SurpriseIndex > 0.85 → 不生成任何任务
	samples := gen.Generate(context.Background(), nil, 0.9)
	if len(samples) != 0 {
		t.Errorf("expected 0 samples when SurpriseIndex > 0.85, got %d", len(samples))
	}
}

// ─── RolloutStore ─────────────────────────────────────────────────────────────

func TestRolloutStore_HardStop(t *testing.T) {
	db := newMemDB(t)
	store, err := optimizer.NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_ = store.SubmitCandidate(ctx, &optimizer.AgentVersionSnapshot{Version: "v1.1"})

	// errorRate > baseline×1.2 → autoRollback
	state, err := store.AdvanceGate(ctx, "v1.1", optimizer.RolloutStats{
		ErrorRate:          0.25,
		BaselineErrorRate:  0.10, // 0.25 > 0.10*1.2=0.12 → hard stop
		P95Latency:         1.0,
		BaselineP95Latency: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != optimizer.RolloutStatusRolledBack {
		t.Errorf("expected rolled_back, got %s", state.Status)
	}
}

func TestRolloutStore_AdvanceGate_Skips24h(t *testing.T) {
	db := newMemDB(t)
	store, _ := optimizer.NewSQLiteRolloutStore(db)

	ctx := context.Background()
	_ = store.SubmitCandidate(ctx, &optimizer.AgentVersionSnapshot{Version: "v1.2"})

	// 正常 stats，但 < 24h → 不推进
	state, err := store.AdvanceGate(ctx, "v1.2", optimizer.RolloutStats{
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

// ─── optimizer.PromptOptimizer ──────────────────────────────────────────────────────────

func TestPromptOptimizer_ReturnsNilOnEmpty(t *testing.T) {
	po := NewPromptOptimizerMVP()
	result := po.Optimize(context.Background(), "task_a", nil)
	if result != nil {
		t.Errorf("expected nil for empty recent, got %v", result)
	}
}

func TestPromptOptimizer_ScoreDescending(t *testing.T) {
	po := NewPromptOptimizerMVP()
	recent := []*optimizer.PromptVersion{
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

// NewPromptOptimizerMVP 创建最简 optimizer.PromptOptimizer（无外部依赖，用于单元测试）。
func NewPromptOptimizerMVP() *optimizer.PromptOptimizer {
	return optimizer.NewPromptOptimizer(nil, nil, 0)
}
