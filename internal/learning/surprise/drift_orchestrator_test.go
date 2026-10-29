package surprise

import (
	"context"
	"testing"
)

func TestDriftOrchestrator_RunOnce_DowngradesAndTriggersReindex(t *testing.T) {
	embedder := &fakeDriftEmbedder{driftFor: map[string]bool{
		"r1": true, "r2": true, "r3": true, "r4": true, "r5": true,
	}}
	dd := NewDriftDetector(168, 0.05, embedder)
	for _, q := range []string{"q1", "q2", "q3", "q4", "q5"} {
		dd.AddAnchor(newAnchor("coding", q))
	}
	for _, q := range []string{"r1", "r2", "r3", "r4", "r5"} {
		dd.AddAnchor(newAnchor("research", q))
	}

	registry := NewDriftDowngradeRegistry()
	reindexCalled := 0
	orchestrator := NewDriftOrchestrator(dd, registry, func(ctx context.Context) (int, bool, error) {
		reindexCalled++
		return 0, false, nil
	}, 0)

	orchestrator.RunOnce(context.Background())

	if registry.IsDowngraded("coding") {
		t.Error("coding should not be downgraded")
	}
	if !registry.IsDowngraded("research") {
		t.Error("research should be downgraded after detecting drift")
	}
	if reindexCalled != 1 {
		t.Errorf("expected reindex trigger to fire once, got %d", reindexCalled)
	}
}

func TestDriftOrchestrator_RunOnce_RecoversWhenDriftClears(t *testing.T) {
	embedder := &fakeDriftEmbedder{driftFor: map[string]bool{"r1": true, "r2": true, "r3": true, "r4": true, "r5": true}}
	dd := NewDriftDetector(168, 0.05, embedder)
	for _, q := range []string{"r1", "r2", "r3", "r4", "r5"} {
		dd.AddAnchor(newAnchor("research", q))
	}
	registry := NewDriftDowngradeRegistry()
	orchestrator := NewDriftOrchestrator(dd, registry, nil, 0)

	orchestrator.RunOnce(context.Background())
	if !registry.IsDowngraded("research") {
		t.Fatal("expected research downgraded on first run")
	}

	// 漂移消失（下一轮 anchor 窗口显示 embedder 恢复一致）：不依赖"reindex 已完成"
	// 的假设，而是直接用新鲜评分覆盖降级状态（RunOnce 文档注释所述设计）。
	embedder.driftFor = map[string]bool{}
	orchestrator.RunOnce(context.Background())
	if registry.IsDowngraded("research") {
		t.Fatal("expected research downgrade cleared once drift no longer detected")
	}
}

func TestDriftOrchestrator_RunOnce_NilSafe(t *testing.T) {
	o := NewDriftOrchestrator(nil, nil, nil, 0)
	o.RunOnce(context.Background()) // 不应 panic
}
