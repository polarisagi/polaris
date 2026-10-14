package pipeline_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
)

type mockValidateDeps struct {
	tools map[string]pipeline.ToolMeta
}

func (m *mockValidateDeps) ToolMeta(name string) pipeline.ToolMeta {
	if t, ok := m.tools[name]; ok {
		return t
	}
	return pipeline.ToolMeta{Name: name, Exists: false}
}

func TestValidate_ValidSpec(t *testing.T) {
	deps := &mockValidateDeps{
		tools: map[string]pipeline.ToolMeta{
			"feed_fetch":       {Name: "feed_fetch", ReadOnly: true, Exists: true},
			"knowledge_ingest": {Name: "knowledge_ingest", WriteAllowed: true, Exists: true},
		},
	}
	lim := pipeline.Limits{
		MaxSteps:           20,
		MinIntervalMin:     15,
		LLMMaxInputChars:   24000,
		LLMMaxOutputTokens: 2000,
		StepDataMaxBytes:   1048576,
	}

	spec := pipeline.Spec{
		Kind: "pipeline",
		Name: "Tech News Pipeline",
		Trigger: pipeline.Trigger{
			Type: "cron",
			Cron: "0 * * * *",
		},
		ResultAction: "session",
		DeliverFrom:  "digest",
		Steps: []pipeline.StepSpec{
			{
				Key:  "fetch",
				Kind: "tool",
				Tool: "feed_fetch",
				Args: json.RawMessage(`{"url":"https://example.com/feed.xml"}`),
			},
			{
				Key:   "merged",
				Kind:  "transform",
				Needs: []string{"fetch"},
				Ops: []pipeline.Op{
					{Op: "concat", From: []string{"fetch"}},
					{Op: "dedup", By: "url", Scope: "workflow"},
					{Op: "filter", Field: "score", Cmp: ">=", Value: 10, KeepMissing: true},
					{Op: "sort", By: "published_at", Desc: true},
					{Op: "limit", N: 10},
					{Op: "pick", Fields: []string{"title", "url"}},
				},
			},
			{
				Key:             "digest",
				Kind:            "llm",
				Needs:           []string{"merged"},
				Input:           "merged",
				Prompt:          "Summarize items",
				Output:          "markdown",
				Thinking:        "off",
				MaxInputChars:   12000,
				MaxOutputTokens: 1200,
			},
		},
	}

	minInterval := func(cron string) (int, error) {
		return 60, nil
	}

	errs := pipeline.Validate(spec, deps, lim, minInterval)
	if len(errs) > 0 {
		t.Fatalf("expected 0 errors, got %d: %+v", len(errs), errs)
	}
}

func TestValidate_InvalidFields(t *testing.T) {
	deps := &mockValidateDeps{
		tools: map[string]pipeline.ToolMeta{
			"bash":             {Name: "bash", Privileged: true, Exists: true},
			"write_file":       {Name: "write_file", ReadOnly: false, WriteAllowed: false, Exists: true},
			"feed_fetch":       {Name: "feed_fetch", ReadOnly: true, Exists: true},
			"knowledge_ingest": {Name: "knowledge_ingest", WriteAllowed: true, Exists: true},
		},
	}
	lim := pipeline.Limits{
		MaxSteps:           5,
		MinIntervalMin:     15,
		LLMMaxInputChars:   10000,
		LLMMaxOutputTokens: 1000,
	}

	spec := pipeline.Spec{
		Name: "", // missing name
		Trigger: pipeline.Trigger{
			Type: "invalid_type",
		},
		ResultAction: "invalid_action",
		DeliverFrom:  "unknown_step",
		Steps: []pipeline.StepSpec{
			{
				Key:  "run", // reserved key
				Kind: "tool",
				Tool: "automation_save", // forbidden
			},
			{
				Key:   "s2",
				Kind:  "tool",
				Tool:  "bash",         // privileged
				Needs: []string{"s3"}, // forward ref
			},
			{
				Key:  "s3",
				Kind: "tool",
				Tool: "write_file", // write tool without hitl
			},
			{
				Key:   "s4",
				Kind:  "transform",
				Needs: []string{"s3"},
				Ops: []pipeline.Op{
					{Op: "concat", From: []string{"nonexistent"}},
					{Op: "filter", Field: "f", Cmp: "regex", Value: "[invalid regex"},
					{Op: "limit", N: 0},
				},
			},
			{
				Key:             "s5",
				Kind:            "llm",
				Needs:           []string{"s4"},
				Input:           "s3", // not in needs
				Prompt:          strings.Repeat("a", 4001),
				Thinking:        "high", // invalid thinking
				MaxInputChars:   15000,  // exceeds limit
				MaxOutputTokens: 2000,   // exceeds limit
			},
			{
				Key:  "s6", // exceeds MaxSteps=5
				Kind: "agent",
			},
		},
	}

	minInterval := func(cron string) (int, error) {
		return 5, errors.New("err")
	}

	errs := pipeline.Validate(spec, deps, lim, minInterval)
	if len(errs) == 0 {
		t.Fatalf("expected validation errors, got 0")
	}

	hasCode := func(path, code string) bool {
		for _, e := range errs {
			if strings.Contains(e.Path, path) && e.Code == code {
				return true
			}
		}
		return false
	}

	if !hasCode("name", "required") {
		t.Errorf("missing name required error")
	}
	if !hasCode("trigger.type", "invalid_enum") {
		t.Errorf("missing trigger.type invalid_enum error")
	}
	if !hasCode("result_action", "invalid_enum") {
		t.Errorf("missing result_action invalid_enum error")
	}
	if !hasCode("deliver_from", "invalid_ref") {
		t.Errorf("missing deliver_from invalid_ref error")
	}
	if !hasCode("steps[0].key", "not_allowed") {
		t.Errorf("missing reserved key error for 'run'")
	}
	if !hasCode("steps[0].tool", "not_allowed") {
		t.Errorf("missing automation_* not_allowed error")
	}
	if !hasCode("steps[1].tool", "not_allowed") {
		t.Errorf("missing privileged tool not_allowed error")
	}
	if !hasCode("steps[1].needs[0]", "invalid_ref") {
		t.Errorf("missing forward ref error for s3")
	}
	if !hasCode("steps[2].tool", "not_allowed") {
		t.Errorf("missing write tool requires approval=hitl error")
	}
	if !hasCode("steps[3].ops[0].from[0]", "invalid_ref") {
		t.Errorf("missing concat from invalid_ref error")
	}
	if !hasCode("steps[3].ops[1].value", "invalid_format") {
		t.Errorf("missing regex invalid_format error")
	}
	if !hasCode("steps[3].ops[2].n", "out_of_range") {
		t.Errorf("missing limit n out_of_range error")
	}
	if !hasCode("steps[4].input", "invalid_ref") {
		t.Errorf("missing llm input invalid_ref error")
	}
	if !hasCode("steps[4].prompt", "out_of_range") {
		t.Errorf("missing llm prompt out_of_range error")
	}
	if !hasCode("steps[4].max_input_chars", "out_of_range") {
		t.Errorf("missing max_input_chars out_of_range error")
	}
	if !hasCode("steps", "out_of_range") {
		t.Errorf("missing steps max_steps out_of_range error")
	}
}

func TestValidate_AdditionalEdgeCases(t *testing.T) {
	deps := &mockValidateDeps{
		tools: map[string]pipeline.ToolMeta{
			"feed_fetch": {Name: "feed_fetch", ReadOnly: true, Exists: true},
		},
	}
	lim := pipeline.Limits{
		MaxSteps:       10,
		MinIntervalMin: 15,
	}

	// 1. deliver_from pointing to tool step
	spec := pipeline.Spec{
		Name:        "Test",
		Trigger:     pipeline.Trigger{Type: "manual"},
		DeliverFrom: "step1",
		Steps: []pipeline.StepSpec{
			{
				Key:  "step1",
				Kind: "tool",
				Tool: "feed_fetch",
				Args: json.RawMessage(`{"nested": {"arr": ["{{steps.unmentioned.count}}"]}}`),
			},
		},
	}

	errs := pipeline.Validate(spec, deps, lim, nil)
	hasCode := func(path, code string) bool {
		for _, e := range errs {
			if strings.Contains(e.Path, path) && e.Code == code {
				return true
			}
		}
		return false
	}

	if !hasCode("deliver_from", "invalid_ref") {
		t.Errorf("expected deliver_from invalid_ref when target is tool step")
	}
	if !hasCode("args", "invalid_ref") {
		t.Errorf("expected args invalid_ref when referencing unmentioned step")
	}

	// 2. Cron 间隔低于限制
	specCron := pipeline.Spec{
		Name: "Cron Test",
		Trigger: pipeline.Trigger{
			Type: "cron",
			Cron: "* * * * *",
		},
		Steps: []pipeline.StepSpec{
			{Key: "step1", Kind: "tool", Tool: "feed_fetch"},
		},
	}
	errsCron := pipeline.Validate(specCron, deps, lim, func(c string) (int, error) { return 5, nil })
	foundIntervalErr := false
	for _, e := range errsCron {
		if e.Path == "trigger.cron" && e.Code == "out_of_range" {
			foundIntervalErr = true
		}
	}
	if !foundIntervalErr {
		t.Errorf("expected out_of_range error for 5 min cron interval")
	}
}
