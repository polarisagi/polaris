package benchmark

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestSWEBenchLiteAdapter_Load(t *testing.T) {
	adapter := &SWEBenchLiteAdapter{}
	if adapter.Name() != "swe-bench" {
		t.Fatalf("expected name swe-bench, got %s", adapter.Name())
	}

	cases, err := adapter.Load(context.Background(), "testdata/swebench_sample.json")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("expected 2 cases, got %d", len(cases))
	}

	// 验证第一条记录映射正确
	c := cases[0]
	if c.ID != "django__django-11099" {
		t.Errorf("expected ID django__django-11099, got %s", c.ID)
	}
	if c.BehaviorType != protocol.BehaviorCodePatch {
		t.Errorf("expected BehaviorCodePatch, got %s", c.BehaviorType)
	}
	if c.Level != protocol.Level3Trajectory {
		t.Errorf("expected Level3Trajectory, got %v", c.Level)
	}
	if c.Source != "swebench-lite" {
		t.Errorf("expected source swebench-lite, got %s", c.Source)
	}
	if c.Description == "" {
		t.Error("expected non-empty Description (problem_statement)")
	}
	// 验证 Input 包含 repo 和 base_commit
	if _, ok := c.Input["repo"]; !ok {
		t.Error("expected Input[repo] to be present")
	}
	if _, ok := c.Input["base_commit"]; !ok {
		t.Error("expected Input[base_commit] to be present")
	}
	// 验证 Expected 包含 patch 和 fail_to_pass
	if _, ok := c.Expected["patch"]; !ok {
		t.Error("expected Expected[patch] to be present")
	}
	failToPass, ok := c.Expected["fail_to_pass"].([]string)
	if !ok || len(failToPass) == 0 {
		t.Error("expected non-empty Expected[fail_to_pass]")
	}
}

func TestSWEBenchLiteAdapter_InvalidPath(t *testing.T) {
	adapter := &SWEBenchLiteAdapter{}
	_, err := adapter.Load(context.Background(), "testdata/nonexistent.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestSWEBenchLiteAdapter_MissingFields(t *testing.T) {
	// GetAdapter 应当返回正确类型
	a := GetAdapter("swe-bench")
	if a == nil {
		t.Fatal("GetAdapter(swe-bench) returned nil")
	}
	if a.Name() != "swe-bench" {
		t.Fatalf("expected swe-bench, got %s", a.Name())
	}
}

func TestGAIAAdapter_Load(t *testing.T) {
	adapter := &GAIAAdapter{}
	if adapter.Name() != "gaia" {
		t.Fatalf("expected name gaia, got %s", adapter.Name())
	}

	cases, err := adapter.Load(context.Background(), "testdata/gaia_sample.jsonl")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(cases))
	}

	// Level 1 → Level1Assert, FalsifiabilityScore=1.0, SeverityP0
	c1 := cases[0]
	if c1.ID != "gaia-level1-001" {
		t.Errorf("expected gaia-level1-001, got %s", c1.ID)
	}
	if c1.BehaviorType != protocol.BehaviorFinalAnswerMatch {
		t.Errorf("expected BehaviorFinalAnswerMatch, got %s", c1.BehaviorType)
	}
	if c1.Level != protocol.Level1Assert {
		t.Errorf("Level 1 should map to Level1Assert, got %v", c1.Level)
	}
	if c1.FalsifiabilityScore != 1.0 {
		t.Errorf("Level 1 falsifiability should be 1.0, got %f", c1.FalsifiabilityScore)
	}
	if c1.Severity != protocol.SeverityP0 {
		t.Errorf("Level 1 severity should be P0, got %s", c1.Severity)
	}
	if c1.Expected["answer"] != "56" {
		t.Errorf("expected answer 56, got %v", c1.Expected["answer"])
	}

	// Level 2 → Level2Schema, SeverityP1
	c2 := cases[1]
	if c2.Level != protocol.Level2Schema {
		t.Errorf("Level 2 should map to Level2Schema, got %v", c2.Level)
	}
	if c2.Severity != protocol.SeverityP1 {
		t.Errorf("Level 2 severity should be P1, got %s", c2.Severity)
	}

	// Level 3 → Level3Trajectory, SeverityP2
	c3 := cases[2]
	if c3.Level != protocol.Level3Trajectory {
		t.Errorf("Level 3 should map to Level3Trajectory, got %v", c3.Level)
	}
	if c3.Severity != protocol.SeverityP2 {
		t.Errorf("Level 3 severity should be P2, got %s", c3.Severity)
	}
}

func TestGAIAAdapter_InvalidPath(t *testing.T) {
	adapter := &GAIAAdapter{}
	_, err := adapter.Load(context.Background(), "testdata/nonexistent.jsonl")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestGetAdapter_NewAdapters(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
	}{
		{"swe-bench", "swe-bench"},
		{"swe-bench-jsonl", "swe-bench-jsonl"},
		{"gaia", "gaia"},
	}
	for _, tt := range tests {
		a := GetAdapter(tt.name)
		if a == nil {
			t.Errorf("GetAdapter(%s) returned nil", tt.name)
			continue
		}
		if a.Name() != tt.wantName {
			t.Errorf("GetAdapter(%s).Name() = %s, want %s", tt.name, a.Name(), tt.wantName)
		}
	}
}
