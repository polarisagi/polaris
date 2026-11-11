package benchmark

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestProjectAdapter_LoadLocalCase(t *testing.T) {
	repo := t.TempDir()
	caseData := protocol.EvalCase{
		ID: "fixture-go", Description: "fix a small bug", BehaviorType: protocol.BehaviorCodePatch,
		Input:    map[string]any{"repo_path": repo, "base_commit": strings.Repeat("a", 40), "source_files": map[string]string{"main.go": "package main\n"}, "test_patch": "trusted"},
		Config:   map[string]any{"fail_to_pass_commands": []string{"go test ./..."}, "pass_to_pass_commands": []string{"go test ./..."}},
		Expected: map[string]any{"criteria": "must stay hidden"},
	}
	data, err := json.Marshal([]protocol.EvalCase{caseData})
	if err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(dataset, data, 0600); err != nil {
		t.Fatal(err)
	}
	cases, err := (ProjectAdapter{}).Load(context.Background(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].ID != caseData.ID {
		t.Fatalf("unexpected cases: %+v", cases)
	}
}

func TestProjectAdapter_UnsupportedWithoutExplicitCommands(t *testing.T) {
	caseData := protocol.EvalCase{
		ID: "unsupported", Description: "fix", BehaviorType: protocol.BehaviorCodePatch,
		Input: map[string]any{"repo_path": "/tmp/repo", "base_commit": strings.Repeat("b", 40), "source_files": map[string]string{"a.go": "package a"}},
	}
	data, err := json.Marshal([]protocol.EvalCase{caseData})
	if err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(dataset, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (ProjectAdapter{}).Load(context.Background(), dataset); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected explicit unsupported error, got %v", err)
	}
}
