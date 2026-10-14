package pipeline_test

import (
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
)

type mockDedupState struct {
	seen    map[string]bool
	pending []string
}

func (m *mockDedupState) Seen(keys []string) (map[string]bool, error) {
	res := make(map[string]bool)
	for _, k := range keys {
		if m.seen[k] {
			res[k] = true
		}
	}
	return res, nil
}

func (m *mockDedupState) Pending(keys []string) {
	m.pending = append(m.pending, keys...)
}

func TestApplyOps_Concat(t *testing.T) {
	inputs := map[string]pipeline.StepData{
		"s1": {Items: []map[string]any{{"id": 1}, {"id": 2}}},
		"s2": {Items: []map[string]any{{"id": 3}}},
	}

	ops := []pipeline.Op{
		{Op: "concat", From: []string{"s1", "s2"}},
	}

	out, err := pipeline.ApplyOps(pipeline.StepData{}, ops, inputs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Count != 3 {
		t.Fatalf("expected 3 items, got %d", out.Count)
	}
	if out.Items[0]["id"] != 1 || out.Items[1]["id"] != 2 || out.Items[2]["id"] != 3 {
		t.Errorf("unexpected concat order: %+v", out.Items)
	}
}

func TestApplyOps_Dedup(t *testing.T) {
	in := pipeline.StepData{
		Items: []map[string]any{
			{"url": "https://a.com", "title": "A1"},
			{"url": "https://a.com", "title": "A2"}, // 重复
			{"url": "https://b.com", "title": "B"},
			{"title": "No URL 1"}, // 缺字段
			{"title": "No URL 2"}, // 缺字段
		},
	}

	// 1. scope=run
	runOps := []pipeline.Op{
		{Op: "dedup", By: "url", Scope: "run"},
	}
	outRun, err := pipeline.ApplyOps(in, runOps, nil, nil)
	if err != nil {
		t.Fatalf("run dedup: %v", err)
	}
	if outRun.Count != 4 {
		t.Fatalf("expected 4 items after run dedup, got %d", outRun.Count)
	}

	// 2. scope=workflow
	dedup := &mockDedupState{
		seen: make(map[string]bool),
	}
	// 第一次运行，应全部纳入并加入 pending
	wfOps := []pipeline.Op{
		{Op: "dedup", By: "url", Scope: "workflow"},
	}
	outWF, err := pipeline.ApplyOps(in, wfOps, nil, dedup)
	if err != nil {
		t.Fatalf("wf dedup: %v", err)
	}
	if outWF.Count != 4 {
		t.Fatalf("expected 4 items after wf dedup, got %d", outWF.Count)
	}
	if len(dedup.pending) != 2 { // a.com 和 b.com 两条
		t.Fatalf("expected 2 pending keys, got %d", len(dedup.pending))
	}

	// 将 pending 标记为 seen
	for _, p := range dedup.pending {
		dedup.seen[p] = true
	}
	dedup.pending = nil

	// 第二次运行，a.com 和 b.com 应被全部过滤，只剩 2 条缺字段的
	outWF2, _ := pipeline.ApplyOps(in, wfOps, nil, dedup)
	if outWF2.Count != 2 {
		t.Fatalf("expected 2 items (missing fields) on second wf run, got %d", outWF2.Count)
	}
}

func TestApplyOps_Filter(t *testing.T) {
	in := pipeline.StepData{
		Items: []map[string]any{
			{"title": "AI Agent News", "likes": 100, "date": "2026-10-14T10:00:00Z"},
			{"title": "Python Tutorial", "likes": 20, "date": "2026-10-10T10:00:00Z"},
			{"title": "Special Topic", "date": "2026-10-12T10:00:00Z"}, // 缺 likes
		},
	}

	// 数值比较 + keep_missing
	ops1 := []pipeline.Op{
		{Op: "filter", Field: "likes", Cmp: ">=", Value: 50, KeepMissing: true},
	}
	out1, err := pipeline.ApplyOps(in, ops1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out1.Count != 2 { // AI Agent News (100) + Special Topic (missing, keepMissing=true)
		t.Fatalf("expected 2 items, got %d", out1.Count)
	}

	// 文本 contains
	ops2 := []pipeline.Op{
		{Op: "filter", Field: "title", Cmp: "contains", Value: "agent"},
	}
	out2, _ := pipeline.ApplyOps(in, ops2, nil, nil)
	if out2.Count != 1 || out2.Items[0]["title"] != "AI Agent News" {
		t.Fatalf("expected 1 item with contains 'agent', got %d", out2.Count)
	}

	// 时间比较
	ops3 := []pipeline.Op{
		{Op: "filter", Field: "date", Cmp: ">", Value: "2026-10-11T00:00:00Z"},
	}
	out3, _ := pipeline.ApplyOps(in, ops3, nil, nil)
	if out3.Count != 2 {
		t.Fatalf("expected 2 items after 2026-10-11, got %d", out3.Count)
	}
}

func TestApplyOps_SortAndLimit(t *testing.T) {
	in := pipeline.StepData{
		Items: []map[string]any{
			{"id": 1, "score": 30},
			{"id": 2, "score": 90},
			{"id": 3}, // 缺 score
			{"id": 4, "score": 60},
		},
	}

	// 降序排序，缺失排最后，取 Top 2
	ops := []pipeline.Op{
		{Op: "sort", By: "score", Desc: true},
		{Op: "limit", N: 2},
	}
	out, err := pipeline.ApplyOps(in, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 {
		t.Fatalf("expected 2 items, got %d", out.Count)
	}
	if out.Items[0]["id"] != 2 || out.Items[1]["id"] != 4 {
		t.Fatalf("expected ids [2, 4], got [%v, %v]", out.Items[0]["id"], out.Items[1]["id"])
	}
}

func TestApplyOps_PickRenameTruncate(t *testing.T) {
	in := pipeline.StepData{
		Items: []map[string]any{
			{
				"meta":    map[string]any{"source": "rss", "extra": "drop_me"},
				"summary": "This is a very long summary text that needs to be truncated.",
				"old_key": "val_123",
				"unused":  "remove_me",
			},
		},
	}

	ops := []pipeline.Op{
		{Op: "rename", Map: map[string]string{"old_key": "new_key"}},
		{Op: "truncate", Field: "summary", MaxChars: 10},
		{Op: "pick", Fields: []string{"meta.source", "summary", "new_key"}},
	}

	out, err := pipeline.ApplyOps(in, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 {
		t.Fatalf("expected 1 item, got %d", out.Count)
	}

	item := out.Items[0]
	if _, ok := item["unused"]; ok {
		t.Errorf("expected unused to be dropped by pick")
	}
	if item["new_key"] != "val_123" {
		t.Errorf("expected new_key=val_123, got %v", item["new_key"])
	}
	if item["summary"] != "This is a " {
		t.Errorf("expected truncated summary 'This is a ', got %q", item["summary"])
	}
	meta, ok := item["meta"].(map[string]any)
	if !ok || meta["source"] != "rss" || meta["extra"] != nil {
		t.Errorf("unexpected picked meta: %+v", meta)
	}
}

func TestApplyOps_FilterAllComparisons(t *testing.T) {
	in := pipeline.StepData{
		Items: []map[string]any{
			{"n": 10, "s": "hello", "t": "2026-10-14T10:00:00Z"},
			{"n": 20, "s": "world", "t": "2026-10-14T12:00:00Z"},
			{"n": "30", "s": "HELLo WORLD", "t": "2026-10-14T14:00:00Z"},
		},
	}

	// 1. not_contains
	out, _ := pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "s", Cmp: "not_contains", Value: "hello"}}, nil, nil)
	if out.Count != 1 || out.Items[0]["s"] != "world" {
		t.Errorf("expected 1 item for not_contains, got %d", out.Count)
	}

	// 2. regex
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "s", Cmp: "regex", Value: `^w.*d$`}}, nil, nil)
	if out.Count != 1 || out.Items[0]["s"] != "world" {
		t.Errorf("expected 1 item for regex, got %d", out.Count)
	}

	// 3. numeric ==, !=, <, <=, >
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "n", Cmp: "==", Value: 10}}, nil, nil)
	if out.Count != 1 {
		t.Errorf("expected 1 item for ==")
	}
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "n", Cmp: "!=", Value: 10}}, nil, nil)
	if out.Count != 2 {
		t.Errorf("expected 2 items for !=")
	}
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "n", Cmp: "<", Value: 20}}, nil, nil)
	if out.Count != 1 {
		t.Errorf("expected 1 item for <")
	}
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "n", Cmp: "<=", Value: 20}}, nil, nil)
	if out.Count != 2 {
		t.Errorf("expected 2 items for <=")
	}
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "n", Cmp: ">", Value: 20}}, nil, nil)
	if out.Count != 1 {
		t.Errorf("expected 1 item for > (string 30 parsed as float)")
	}

	// 4. time <=, >=, ==, !=
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "t", Cmp: "<=", Value: "2026-10-14T12:00:00Z"}}, nil, nil)
	if out.Count != 2 {
		t.Errorf("expected 2 items for time <=")
	}
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "t", Cmp: "==", Value: "2026-10-14T10:00:00Z"}}, nil, nil)
	if out.Count != 1 {
		t.Errorf("expected 1 item for time ==")
	}
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "t", Cmp: "!=", Value: "2026-10-14T10:00:00Z"}}, nil, nil)
	if out.Count != 2 {
		t.Errorf("expected 2 items for time !=")
	}

	// 5. string comparison
	out, _ = pipeline.ApplyOps(in, []pipeline.Op{{Op: "filter", Field: "s", Cmp: ">", Value: "zebra"}}, nil, nil)
	if out.Count != 0 {
		t.Errorf("expected 0 items for string > zebra")
	}

	// 6. nested rename
	nestedIn := pipeline.StepData{
		Items: []map[string]any{
			{"deep": map[string]any{"nested": "old"}},
		},
	}
	outNested, _ := pipeline.ApplyOps(nestedIn, []pipeline.Op{{Op: "rename", Map: map[string]string{"deep.nested": "deep.renamed"}}}, nil, nil)
	deepMap := outNested.Items[0]["deep"].(map[string]any)
	if deepMap["renamed"] != "old" || deepMap["nested"] != nil {
		t.Errorf("unexpected nested rename: %+v", deepMap)
	}

	// 7. unknown op returns error
	_, err := pipeline.ApplyOps(in, []pipeline.Op{{Op: "unsupported"}}, nil, nil)
	if err == nil {
		t.Errorf("expected error for unsupported op")
	}
}

func TestApplyOps_SortTimeAndStrings(t *testing.T) {
	in := pipeline.StepData{
		Items: []map[string]any{
			{"t": "2026-10-14T15:00:00Z", "s": "banana"},
			{"t": "2026-10-14T10:00:00Z", "s": "apple"},
			{"t": "2026-10-14T12:00:00Z", "s": "cherry"},
		},
	}

	// 升序时间
	outT, _ := pipeline.ApplyOps(in, []pipeline.Op{{Op: "sort", By: "t"}}, nil, nil)
	if outT.Items[0]["s"] != "apple" || outT.Items[2]["s"] != "banana" {
		t.Errorf("unexpected time sort result: %+v", outT.Items)
	}

	// 降序字符串
	outS, _ := pipeline.ApplyOps(in, []pipeline.Op{{Op: "sort", By: "s", Desc: true}}, nil, nil)
	if outS.Items[0]["s"] != "cherry" || outS.Items[2]["s"] != "apple" {
		t.Errorf("unexpected string sort result: %+v", outS.Items)
	}
}
