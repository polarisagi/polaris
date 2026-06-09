package cognition

import (
	"context"
	"strings"
	"testing"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

type mockWasmExecutor struct {
	res []byte
	err error
}

func (m *mockWasmExecutor) ExecuteTest(ctx context.Context, wasmBytes []byte, input []byte) ([]byte, error) {
	return m.res, m.err
}

// TestWasmTester_Run_NoTestCases — 空列表直接通过
func TestWasmTester_Run_NoTestCases(t *testing.T) {
	wt := &WasmTester{}
	if err := wt.Run(); err != nil {
		t.Errorf("Expected nil error for empty test cases, got %v", err)
	}
}

// TestWasmTester_Run_PassesOnMatchingOutput — 输出匹配时 nil
func TestWasmTester_Run_PassesOnMatchingOutput(t *testing.T) {
	mock := &mockWasmExecutor{res: []byte("success")}
	wt := &WasmTester{
		runtime: mock,
		testCases: []TestCase{
			{Name: "case1", Input: []byte("in"), Expect: []byte("success")},
		},
	}
	if err := wt.Run(); err != nil {
		t.Errorf("Expected nil error on match, got %v", err)
	}
}

// TestWasmTester_Run_FailsOnMismatch — 输出不匹配时有描述性错误
func TestWasmTester_Run_FailsOnMismatch(t *testing.T) {
	mock := &mockWasmExecutor{res: []byte("wrong")}
	wt := &WasmTester{
		runtime: mock,
		testCases: []TestCase{
			{Name: "case1", Input: []byte("in"), Expect: []byte("success")},
		},
	}
	err := wt.Run()
	if err == nil {
		t.Fatalf("Expected error on mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "输出不匹配") {
		t.Errorf("Expected descriptive error containing '输出不匹配', got %v", err)
	}
}

// TestWasmTester_Run_FailsOnExecutionError — 执行错误时有包装错误
func TestWasmTester_Run_FailsOnExecutionError(t *testing.T) {
	mock := &mockWasmExecutor{err: perrors.New(perrors.CodeInternal, "sandbox crash")}
	wt := &WasmTester{
		runtime: mock,
		testCases: []TestCase{
			{Name: "case1", Input: []byte("in"), Expect: []byte("success")},
		},
	}
	err := wt.Run()
	if err == nil {
		t.Fatalf("Expected error on execution failure, got nil")
	}
	if !strings.Contains(err.Error(), "Wasm 行为测试执行失败") {
		t.Errorf("Expected wrapped error, got %v", err)
	}
}
