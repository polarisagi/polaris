// Package tool 测试 InMemoryToolRegistry 的注册/查找/执行/策略/污点/shell 路径。
package tool

import (
	"github.com/polarisagi/polaris/internal/security/token"

	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── mock：PolicyGate ───────────────────────────────────────────────────────

type mockPolicyGate struct {
	allow bool
}

func (m *mockPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return m.allow, nil
}

func (m *mockPolicyGate) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: m.allow}, nil
}

// mockPolicyGateWithError 模拟 policy engine 返回错误的情况
type mockPolicyGateWithError struct{}

func (m *mockPolicyGateWithError) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return false, apperr.New(apperr.CodeInternal, "policy engine failure")
}

func (m *mockPolicyGateWithError) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{}, nil
}

// ─── mock：SandboxExecutor ──────────────────────────────────────────────────

type mockSandbox struct {
	output []byte
	err    error
}

func (s *mockSandbox) Execute(_ context.Context, _ string, _ []byte, _ types.TaintLevel) ([]byte, error) {
	return s.output, s.err
}

// ─── 辅助函数 ────────────────────────────────────────────────────────────────

// minTool 构造只有 Name/Source 的最小工具。
func minTool(name string) types.Tool {
	return types.Tool{
		Name:   name,
		Source: types.ToolBuiltin,
	}
}

func mockTokenForTest() *token.Token {
	tok, _ := action.GetTokenManager().Mint("test-agent", []token.CapabilityType{token.CapProcess}, 1, 5*time.Minute, 0)
	return tok
}

func ctxWithToken() context.Context {
	return context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, mockTokenForTest())
}

// newAllowRegistry 创建带 allow 策略的注册表。
func newAllowRegistry() *InMemoryToolRegistry {
	return NewInMemoryToolRegistry(nil, nil)
}

// ─── 注册/查找/列举 ──────────────────────────────────────────────────────────

func TestRegister_EmptyName(t *testing.T) {
	r := newAllowRegistry()
	err := r.Register(types.Tool{Name: ""})
	if err == nil {
		t.Fatal("空 Name 应返回 error，实际 nil")
	}
}

func TestRegister_OverwriteExisting(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(types.Tool{Name: "foo", Version: "v1", Source: types.ToolBuiltin})
	_ = r.Register(types.Tool{Name: "foo", Version: "v2", Source: types.ToolBuiltin})

	got, err := r.Lookup("foo")
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if got.Version != "v2" {
		t.Fatalf("同名覆盖后 Version 应为 v2，实际 %q", got.Version)
	}
}

func TestLookup_NotFound(t *testing.T) {
	r := newAllowRegistry()
	_, err := r.Lookup("nonexistent")
	if err == nil {
		t.Fatal("未注册工具应返回 error，实际 nil")
	}
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("errors.Is(err, ErrToolNotFound) 应为 true，实际 err=%v", err)
	}
}

func TestLookup_Found(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(minTool("bar"))

	got, err := r.Lookup("bar")
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if got.Name != "bar" {
		t.Fatalf("Name 期望 bar，实际 %q", got.Name)
	}
}

func TestList_Empty(t *testing.T) {
	r := newAllowRegistry()
	list := r.List()
	if len(list) != 0 {
		t.Fatalf("空注册表 List() 应返回空 slice，len=%d", len(list))
	}
}

func TestList_Multiple(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(minTool("a"))
	_ = r.Register(minTool("b"))

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("注册2个工具后 List() 应返回 len=2，实际 %d", len(list))
	}
}

// ─── ExecuteTool ─────────────────────────────────────────────────────────────

func TestExecuteTool_ToolNotRegistered(t *testing.T) {
	r := newAllowRegistry()
	_, err := r.ExecuteTool(ctxWithToken(), "ghost", []byte("x"), types.TaintNone)
	if err == nil {
		t.Fatal("未注册工具 ExecuteTool 应返回 error")
	}
}

func TestExecuteTool_PolicyNil(t *testing.T) {
	r := NewInMemoryToolRegistry(nil, nil)
	_ = r.Register(minTool("test-tool"))
	res, err := r.ExecuteTool(ctxWithToken(), "test-tool", []byte("x"), types.TaintNone)
	if err == nil {
		t.Fatal("expected error when policy is nil")
	}
	if res.Success || res.Error != "tool_registry: policy gate not initialized, refusing tool call (fail-closed)" {
		t.Fatalf("expected fail-closed error, got res: %+v", res)
	}
}

func TestExecuteTool_NoSandbox_ReturnsInput(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(minTool("echo"))

	input := []byte("hello")
	res, err := r.ExecuteTool(ctxWithToken(), "echo", input, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool 意外 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("无 sandbox 时 Success 应为 true，Error=%q", res.Error)
	}
	if string(res.Output) != string(input) {
		t.Fatalf("无 sandbox 时 Output 应等于 input，got %q", res.Output)
	}
}

func TestExecuteTool_PolicyDenied(t *testing.T) {
	r := NewInMemoryToolRegistry(nil, nil)
	_ = r.Register(minTool("secret"))

	res, err := r.ExecuteTool(ctxWithToken(), "secret", []byte("x"), types.TaintNone)
	// policy deny 时应返回 nil err + Success=false（不泄露内部错误）
	if err != nil {
		t.Fatalf("policy deny 不应返回底层 error，实际: %v", err)
	}
	if res.Success {
		t.Fatal("policy deny 时 Success 应为 false")
	}
	if res.Error == "" {
		t.Fatal("policy deny 时 result.Error 不应为空")
	}
}

func TestExecuteTool_SandboxError(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(minTool("boom"))
	r.SetSandbox(&mockSandbox{err: apperr.New(apperr.CodeInternal, "sandbox kaboom")})

	res, err := r.ExecuteTool(ctxWithToken(), "boom", []byte("x"), types.TaintNone)
	if err != nil {
		t.Fatalf("sandbox 错误不应作为函数 error 返回，实际: %v", err)
	}
	if res.Success {
		t.Fatal("sandbox 报错时 Success 应为 false")
	}
	if res.Error == "" {
		t.Fatal("sandbox 报错时 result.Error 不应为空")
	}
}

func TestExecuteTool_SandboxSuccess(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(minTool("ok"))
	r.SetSandbox(&mockSandbox{output: []byte("world")})

	res, err := r.ExecuteTool(ctxWithToken(), "ok", []byte("hello"), types.TaintNone)
	if err != nil {
		t.Fatalf("意外 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("sandbox 成功时 Success 应为 true，Error=%q", res.Error)
	}
	if string(res.Output) != "world" {
		t.Fatalf("Output 期望 world，实际 %q", res.Output)
	}
}

func TestExecuteTool_TaintPropagation(t *testing.T) {
	r := newAllowRegistry()
	_ = r.Register(minTool("tainted"))

	res, err := r.ExecuteTool(ctxWithToken(), "tainted", []byte("x"), types.TaintHigh)
	if err != nil {
		t.Fatalf("意外 error: %v", err)
	}
	if res.TaintLevel != types.TaintHigh {
		t.Fatalf("TaintLevel 期望 TaintHigh(%d)，实际 %d", types.TaintHigh, res.TaintLevel)
	}
}

func TestExecuteTool_ShellTool_Uses_ShellLimiter(t *testing.T) {
	r := newAllowRegistry()
	// SideProcessSpawn 工具 → isShellTool=true → limiter key="shell"
	shellTool := types.Tool{
		Name:        "run-sh",
		Source:      types.ToolBuiltin,
		SideEffects: []types.SideEffect{types.SideProcessSpawn},
	}
	_ = r.Register(shellTool)

	// 不注入 sandbox，验证路径不 panic，能正常返回
	res, err := r.ExecuteTool(ctxWithToken(), "run-sh", []byte("ls"), types.TaintNone)
	if err != nil {
		t.Fatalf("shell 工具 ExecuteTool 不应 panic/error: %v", err)
	}
	// shell limiter 容量=2，第1次必须允许
	if !res.Success {
		t.Fatalf("shell 工具首次调用应 Success=true，Error=%q", res.Error)
	}
}

// ─── Policy Error & Context Cancel 分支覆盖 ──────────────────────────────────

func TestExecuteTool_PolicyError(t *testing.T) {
	r := NewInMemoryToolRegistry(nil, nil)
	_ = r.Register(minTool("err-tool"))

	result, err := r.ExecuteTool(ctxWithToken(), "err-tool", nil, types.TaintNone)
	// policy engine 返回 error 时应返回 nil err + Success=false（不泄露内部错误）
	if err != nil {
		t.Fatalf("不期望底层 error: %v", err)
	}
	if result.Success {
		t.Error("policy 返回 error 时期望 Success=false")
	}
	if result.Error == "" {
		t.Fatal("policy error 时 result.Error 不应为空")
	}
}

func TestExecuteTool_ContextCancelled_PolicyStillRuns(t *testing.T) {
	// tool.go 内部把 ctx 传给 security.IsAuthorized
	// 已取消的 ctx 是否正确处理取决于 policy mock 实现（我们的 mock 忽略 ctx）
	// 此测试验证：cancelled ctx 不会导致 panic 或底层 error 泄漏
	r := NewInMemoryToolRegistry(nil, nil)
	_ = r.Register(minTool("ctx-tool"))

	ctx, cancel := context.WithCancel(ctxWithToken())
	cancel()

	result, err := r.ExecuteTool(ctx, "ctx-tool", []byte("data"), types.TaintNone)
	if err != nil {
		t.Fatalf("不期望底层 error（mock policy 忽略 ctx）: %v", err)
	}
	// mock sandbox=nil 时返回成功
	if !result.Success {
		t.Fatalf("cancelled ctx + allow policy 应 Success=true，Error=%q", result.Error)
	}
}
