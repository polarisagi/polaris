package surprise

import "testing"

// fakeDriftEmbedder 供测试使用的确定性 Embedder：query 命中 driftFor 时返回与
// stored embedding 正交的向量（模拟漂移），否则返回与 stored embedding 相同的
// 向量（模拟无漂移）。
type fakeDriftEmbedder struct {
	driftFor map[string]bool
}

func (f *fakeDriftEmbedder) Embed(text string) []float32 {
	if f.driftFor[text] {
		return []float32{0, 1, 0}
	}
	return []float32{1, 0, 0}
}

func newAnchor(taskType, query string) AnchorSample {
	return AnchorSample{
		TaskType:  taskType,
		Query:     query,
		Embedding: []float32{1, 0, 0},
		Expected:  []string{"result_1"},
	}
}

func TestDriftDetector_DetectByTaskType_PerTaskGrouping(t *testing.T) {
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

	reports := dd.DetectByTaskType()
	if len(reports) != 2 {
		t.Fatalf("expected 2 task_type reports, got %d", len(reports))
	}
	if reports["coding"].NeedsReindex {
		t.Errorf("coding group should not need reindex: %+v", reports["coding"])
	}
	if !reports["research"].NeedsReindex {
		t.Errorf("research group should need reindex: %+v", reports["research"])
	}
}

func TestDriftDetector_DetectByTaskType_SkipsSmallGroups(t *testing.T) {
	embedder := &fakeDriftEmbedder{}
	dd := NewDriftDetector(168, 0.05, embedder)
	dd.AddAnchor(newAnchor("rare_type", "q1"))
	dd.AddAnchor(newAnchor("rare_type", "q2"))

	reports := dd.DetectByTaskType()
	if _, ok := reports["rare_type"]; ok {
		t.Errorf("groups with <5 anchors should be skipped, got report for rare_type")
	}
}

func TestDriftDetector_RecordAnchor_MatchesAddAnchor(t *testing.T) {
	dd := NewDriftDetector(168, 0.05, &fakeDriftEmbedder{})
	dd.RecordAnchor("debug", "why does it crash", []float32{1, 2, 3}, []string{"a", "b"})

	if len(dd.anchors) != 1 {
		t.Fatalf("expected 1 anchor recorded, got %d", len(dd.anchors))
	}
	got := dd.anchors[0]
	if got.TaskType != "debug" || got.Query != "why does it crash" || len(got.Expected) != 2 {
		t.Errorf("unexpected anchor recorded: %+v", got)
	}
}

func TestDriftDowngradeRegistry(t *testing.T) {
	r := NewDriftDowngradeRegistry()
	if r.IsDowngraded("coding") {
		t.Fatal("fresh registry should report no downgrades")
	}
	if r.IsDowngraded("") {
		t.Fatal("empty task_type should never be downgraded")
	}

	r.SetDowngraded("coding", true)
	if !r.IsDowngraded("coding") {
		t.Fatal("expected coding to be downgraded")
	}
	if got := r.Downgraded(); len(got) != 1 || got[0] != "coding" {
		t.Fatalf("unexpected Downgraded() result: %v", got)
	}

	r.SetDowngraded("coding", false)
	if r.IsDowngraded("coding") {
		t.Fatal("expected coding downgrade to clear")
	}

	r.SetDowngraded("research", true)
	r.SetDowngraded("debug", true)
	r.ClearAll()
	if len(r.Downgraded()) != 0 {
		t.Fatal("ClearAll should remove all downgrade flags")
	}
}
