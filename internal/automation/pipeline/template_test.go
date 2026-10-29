package pipeline_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockRefResolver struct {
	steps map[string]pipeline.StepData
	run   map[string]string
}

func (m *mockRefResolver) Step(key string) (pipeline.StepData, bool) {
	d, ok := m.steps[key]
	return d, ok
}

func (m *mockRefResolver) Run(name string) (string, bool) {
	s, ok := m.run[name]
	return s, ok
}

func TestRender_ExactReplacements(t *testing.T) {
	r := &mockRefResolver{
		steps: map[string]pipeline.StepData{
			"feed": {
				Items: []map[string]any{{"title": "Item 1", "url": "https://example.com/1"}},
				Count: 1,
				Text:  "Raw text from feed",
			},
		},
		run: map[string]string{
			"date":  "2026-10-14",
			"since": "2026-10-13T00:00:00Z",
		},
	}

	raw := json.RawMessage(`{
		"docs": "{{steps.feed.items}}",
		"total": "{{steps.feed.count}}",
		"text": "{{steps.feed.text}}",
		"date": "{{run.date}}",
		"since": "{{run.since}}"
	}`)

	out, refsUsed, err := pipeline.Render(raw, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !refsUsed {
		t.Fatalf("expected refsUsed=true")
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal rendered json: %v", err)
	}

	docs, ok := parsed["docs"].([]any)
	if !ok || len(docs) != 1 {
		t.Errorf("expected docs to be array of len 1, got %v", parsed["docs"])
	}
	total, ok := parsed["total"].(float64)
	if !ok || int(total) != 1 {
		t.Errorf("expected total=1, got %v", parsed["total"])
	}
	if parsed["text"] != "Raw text from feed" {
		t.Errorf("expected text 'Raw text from feed', got %v", parsed["text"])
	}
	if parsed["date"] != "2026-10-14" {
		t.Errorf("expected date 2026-10-14, got %v", parsed["date"])
	}
	if parsed["since"] != "2026-10-13T00:00:00Z" {
		t.Errorf("expected since 2026-10-13T00:00:00Z, got %v", parsed["since"])
	}
}

func TestRender_StringInterpolation(t *testing.T) {
	r := &mockRefResolver{
		steps: map[string]pipeline.StepData{
			"stats": {
				Count: 42,
				Text:  "ok",
			},
		},
		run: map[string]string{
			"date": "2026-10-14",
		},
	}

	raw := json.RawMessage(`{
		"query": "date={{run.date}}&count={{steps.stats.count}}",
		"static": "no templates here"
	}`)

	out, refsUsed, err := pipeline.Render(raw, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !refsUsed {
		t.Fatalf("expected refsUsed=true")
	}

	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if parsed["query"] != "date=2026-10-14&count=42" {
		t.Errorf("unexpected query string: %v", parsed["query"])
	}
	if parsed["static"] != "no templates here" {
		t.Errorf("unexpected static string: %v", parsed["static"])
	}
}

func TestRender_Errors(t *testing.T) {
	r := &mockRefResolver{
		steps: map[string]pipeline.StepData{},
		run:   map[string]string{},
	}

	// 1. 未知步骤
	_, _, err := pipeline.Render(json.RawMessage(`{"items": "{{steps.missing.items}}"}`), r)
	if err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for missing step, got %v", err)
	}

	// 2. 未知运行变量
	_, _, err = pipeline.Render(json.RawMessage(`{"date": "{{run.unknown}}"}`), r)
	if err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for unknown run variable, got %v", err)
	}

	// 3. 超过 64KiB
	bigResolver := &mockRefResolver{
		steps: map[string]pipeline.StepData{},
		run: map[string]string{
			"big": strings.Repeat("A", 70000),
		},
	}
	_, _, err = pipeline.Render(json.RawMessage(`{"msg": "prefix_{{run.big}}"}`), bigResolver)
	if err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for oversized string, got %v", err)
	}

	// 4. 空或者纯静态输入
	staticOut, used, err := pipeline.Render(json.RawMessage(`{"k":"v"}`), r)
	if err != nil || used || string(staticOut) != `{"k":"v"}` {
		t.Errorf("expected static unchanged, used=false")
	}

	// 5. 嵌套结构与对象转换为字符串插值
	rNested := &mockRefResolver{
		steps: map[string]pipeline.StepData{
			"s": {
				Items: []map[string]any{{"a": 1}},
				Count: 5,
			},
		},
		run: map[string]string{"date": "2026-10-14"},
	}
	nestedRaw := json.RawMessage(`{
		"list": [
			{"inner": "date is {{run.date}} and items: {{steps.s.items}}"}
		]
	}`)
	nestedOut, used, err := pipeline.Render(nestedRaw, rNested)
	if err != nil || !used {
		t.Fatalf("expected successful nested render with used=true, got err=%v", err)
	}
	if !strings.Contains(string(nestedOut), "2026-10-14") || !strings.Contains(string(nestedOut), `[{\"a\":1}]`) {
		t.Errorf("unexpected nested output: %s", string(nestedOut))
	}
}
