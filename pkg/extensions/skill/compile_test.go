package skill

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// ─── TaintSanitizeForRemoteCompilation ───────────────────────────────────────

func TestTaintSanitizeForRemoteCompilation_ReplacesLiterals(t *testing.T) {
	src := []byte(`package main

import "fmt"

func run() {
	name := "Alice"
	age := 30
	fmt.Printf("%s is %d years old", name, age)
}
`)
	sanitized, rm, err := TaintSanitizeForRemoteCompilation(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rm.Params) == 0 {
		t.Fatal("redaction map should not be empty")
	}
	// 脱敏后不应含原始字符串
	if contains(sanitized, "Alice") {
		t.Error("sanitized source should not contain original string 'Alice'")
	}
	// 导入路径必须保留
	if !contains(sanitized, `"fmt"`) {
		t.Error("import path 'fmt' must be preserved")
	}
}

func TestTaintSanitizeForRemoteCompilation_PreservesImportPaths(t *testing.T) {
	src := []byte(`package main

import (
	"encoding/json"
	"fmt"
)

func main() {
	data := "test"
	_ = json.Marshal(data)
	fmt.Println("hello")
}
`)
	sanitized, _, err := TaintSanitizeForRemoteCompilation(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(sanitized, `"encoding/json"`) {
		t.Error("import path encoding/json must be preserved")
	}
	if !contains(sanitized, `"fmt"`) {
		t.Error("import path fmt must be preserved")
	}
}

func TestTaintSanitizeForRemoteCompilation_InvalidGo(t *testing.T) {
	src := []byte(`this is not valid Go code {{{`)
	_, _, err := TaintSanitizeForRemoteCompilation(src)
	if err == nil {
		t.Error("expected error for invalid Go source")
	}
}

// ─── CompileGate ─────────────────────────────────────────────────────────────

func TestCompileGate_Tier0_Serial(t *testing.T) {
	gate := NewCompileGate(observability.Tier0)
	// 第一个应该成功（内存充足）
	if !gate.TryAcquire(500) {
		t.Fatal("first acquire should succeed")
	}
	// 第二个应该被拒绝（串行限制）
	if gate.TryAcquire(500) {
		t.Error("second acquire on Tier0 gate should fail (serial)")
	}
	gate.Release()
	// 释放后可再次获取
	if !gate.TryAcquire(500) {
		t.Error("after release, acquire should succeed again")
	}
	gate.Release()
}

func TestCompileGate_Tier1_MaxTwo(t *testing.T) {
	gate := NewCompileGate(observability.Tier1)
	if !gate.TryAcquire(500) {
		t.Fatal("first acquire should succeed")
	}
	if !gate.TryAcquire(500) {
		t.Fatal("second acquire on Tier1 should succeed")
	}
	if gate.TryAcquire(500) {
		t.Error("third acquire on Tier1 should fail")
	}
	gate.Release()
	gate.Release()
}

func TestCompileGate_MemoryPressure_Rejects(t *testing.T) {
	gate := NewCompileGate(observability.Tier2)
	// 内存不足（< 80MB）
	if gate.TryAcquire(50) {
		t.Error("TryAcquire should fail when freeMB < 80")
	}
}

func TestCompileGate_InFlight(t *testing.T) {
	gate := NewCompileGate(observability.Tier2)
	if gate.InFlight() != 0 {
		t.Fatal("initial InFlight should be 0")
	}
	gate.TryAcquire(500)
	gate.TryAcquire(500)
	if gate.InFlight() != 2 {
		t.Fatalf("expected InFlight=2, got %d", gate.InFlight())
	}
	gate.Release()
	if gate.InFlight() != 1 {
		t.Fatalf("expected InFlight=1, got %d", gate.InFlight())
	}
	gate.Release()
}

// ─── FreshnessChecker ────────────────────────────────────────────────────────

func TestFreshnessChecker_Fresh(t *testing.T) {
	fc := &FreshnessChecker{}
	now := time.Now().Unix()
	traj := &CollapseTrajectory{
		CompletedAt: now,
		Entities: []CollapseEntity{
			{ID: "e1", UpdatedAt: now - 3600}, // 更新早于完成时间 → fresh
		},
	}
	result, err := fc.Check(context.Background(), traj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Fresh {
		t.Error("trajectory should be fresh")
	}
}

func TestFreshnessChecker_Stale(t *testing.T) {
	fc := &FreshnessChecker{}
	now := time.Now().Unix()
	traj := &CollapseTrajectory{
		CompletedAt: now - 3600,
		Entities: []CollapseEntity{
			{ID: "e1", UpdatedAt: now}, // 实体在轨迹完成后被更新 → stale
		},
	}
	result, err := fc.Check(context.Background(), traj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Fresh {
		t.Error("trajectory should be stale")
	}
	if len(result.Stale) != 1 || result.Stale[0].ID != "e1" {
		t.Error("stale entity should be reported")
	}
}

func TestFreshnessChecker_NoEntities_IsFresh(t *testing.T) {
	fc := &FreshnessChecker{}
	traj := &CollapseTrajectory{Entities: nil}
	result, err := fc.Check(context.Background(), traj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Fresh {
		t.Error("empty entities should be fresh")
	}
}

// ─── StaticCFGAnalyzer ────────────────────────────────────────────────────────

func TestStaticCFGAnalyzer_ForbiddenImport(t *testing.T) {
	src := []byte(`package main

import "os/exec"

func main() {
	exec.Command("ls").Run()
}
`)
	sa := &StaticCFGAnalyzer{}
	result, err := sa.Analyze(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Error("should detect forbidden import os/exec")
	}
	if !containsStr(result.Violations, "os/exec") {
		t.Errorf("violations should mention os/exec, got: %v", result.Violations)
	}
}

func TestStaticCFGAnalyzer_NonDeterministicCall(t *testing.T) {
	src := []byte(`package main

import "time"

func run() string {
	return time.Now().String()
}
`)
	sa := &StaticCFGAnalyzer{}
	result, err := sa.Analyze(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Error("should detect time.Now() non-deterministic call")
	}
}

func TestStaticCFGAnalyzer_CleanCode(t *testing.T) {
	src := []byte(`package main

import "strings"

func run(input string) string {
	return strings.ToUpper(input)
}
`)
	sa := &StaticCFGAnalyzer{}
	result, err := sa.Analyze(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Errorf("clean code should pass analysis, violations: %v", result.Violations)
	}
}

// ─── validateWasmMagic ────────────────────────────────────────────────────────

func TestValidateWasmMagic_Valid(t *testing.T) {
	valid := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	if err := validateWasmMagic(valid); err != nil {
		t.Errorf("valid wasm magic should pass: %v", err)
	}
}

func TestValidateWasmMagic_Invalid(t *testing.T) {
	invalid := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	if err := validateWasmMagic(invalid); err == nil {
		t.Error("invalid magic should fail")
	}
}

func TestValidateWasmMagic_TooShort(t *testing.T) {
	if err := validateWasmMagic([]byte{0x00, 0x61}); err == nil {
		t.Error("too-short bytes should fail")
	}
}

// ─── DataStripper ─────────────────────────────────────────────────────────────

func TestDataStripper_RemovesEntitiesAndTimestamp(t *testing.T) {
	ds := &DataStripper{}
	traj := &CollapseTrajectory{
		SkillID:     "test-skill",
		CompletedAt: 12345,
		Entities:    []CollapseEntity{{ID: "e1"}},
		ToolCalls:   []CollapseToolCall{{ToolName: "search", Args: map[string]string{"q": "string"}}},
	}
	stripped := ds.Strip(traj)
	if stripped.CompletedAt != 0 {
		t.Error("CompletedAt should be zeroed")
	}
	if len(stripped.Entities) != 0 {
		t.Error("Entities should be stripped")
	}
	if stripped.SkillID != "test-skill" {
		t.Error("SkillID should be preserved")
	}
	if len(stripped.ToolCalls) != 1 || stripped.ToolCalls[0].ToolName != "search" {
		t.Error("ToolCalls should be preserved (type-only)")
	}
}

// ─── RedactionMap ─────────────────────────────────────────────────────────────

func TestRedactionMap_SequentialKeys(t *testing.T) {
	rm := &RedactionMap{Params: make(map[string]string)}
	k0 := rm.next()
	k1 := rm.next()
	k2 := rm.next()
	if k0 != "param_0" || k1 != "param_1" || k2 != "param_2" {
		t.Errorf("expected param_0..2, got %s %s %s", k0, k1, k2)
	}
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

func contains(data []byte, s string) bool {
	return len(data) > 0 && len(s) > 0 &&
		(func() bool {
			str := string(data)
			for i := 0; i <= len(str)-len(s); i++ {
				if str[i:i+len(s)] == s {
					return true
				}
			}
			return false
		})()
}

func containsStr(slice []string, substr string) bool {
	for _, s := range slice {
		if len(s) >= len(substr) {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
		}
	}
	return false
}
