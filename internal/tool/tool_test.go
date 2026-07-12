// Package tool 测试 InMemoryToolRegistry 的注册/查找/执行/策略/污点/shell 路径。
package tool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
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

// ─── 辅助函数 ────────────────────────────────────────────────────────────────

// minTool 构造只有 Name/Source 的最小工具。
func minTool(name string) types.Tool {
	return types.Tool{
		Name:      name,
		Source:    types.ToolBuiltin,
		TrustTier: types.TrustSystem,
	}
}

func mockTokenForTest() *token.Token {
	tok, _ := action.GetTokenManager().Mint("test-agent", []token.CapabilityType{token.CapProcess}, 1, 5*time.Minute, 0)
	return tok
}

func ctxWithToken() context.Context {
	return context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, mockTokenForTest())
}

func newAllowRegistry() (*InMemoryToolRegistry, *sandbox.InProcessSandbox) {
	sbx := sandbox.NewInProcessSandbox()
	router := sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0)
	return NewInMemoryToolRegistry(sandbox.NewExecEnvelope(&mockPolicyGate{allow: true}, router, 0, "linux", nil)), sbx
}

// ─── 注册/查找/列举 ──────────────────────────────────────────────────────────

func TestRegister_EmptyName(t *testing.T) {
	r, _ := newAllowRegistry()
	err := r.Register(types.Tool{Name: ""})
	if err == nil {
		t.Fatal("空 Name 应返回 error，实际 nil")
	}
}

func TestRegister_OverwriteExisting(t *testing.T) {
	r, _ := newAllowRegistry()
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
	r, _ := newAllowRegistry()
	_, err := r.Lookup("nonexistent")
	if err == nil {
		t.Fatal("未注册工具应返回 error，实际 nil")
	}
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("errors.Is(err, ErrToolNotFound) 应为 true，实际 err=%v", err)
	}
}

func TestLookup_Found(t *testing.T) {
	r, _ := newAllowRegistry()
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
	r, _ := newAllowRegistry()
	list := r.List()
	if len(list) != 0 {
		t.Fatalf("空注册表 List() 应返回空 slice，len=%d", len(list))
	}
}

func TestList_Multiple(t *testing.T) {
	r, _ := newAllowRegistry()
	_ = r.Register(minTool("a"))
	_ = r.Register(minTool("b"))

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("注册2个工具后 List() 应返回 len=2，实际 %d", len(list))
	}
}

// ─── ExecuteTool ─────────────────────────────────────────────────────────────

func TestExecuteTool_ToolNotRegistered(t *testing.T) {
	r, _ := newAllowRegistry()
	_, err := r.ExecuteTool(ctxWithToken(), "ghost", []byte("x"), types.TaintNone)
	if err == nil {
		t.Fatal("未注册工具 ExecuteTool 应返回 error")
	}
}

func TestExecuteTool_PolicyNil(t *testing.T) {
	r := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(nil, sandbox.NewSandboxRouter(sandbox.NewInProcessSandbox(), nil, nil, "linux", 0), 0, "linux", nil))
	_ = r.Register(minTool("test-tool"))
	res, err := r.ExecuteTool(ctxWithToken(), "test-tool", []byte("x"), types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool 意外 error: %v", err)
	}
	if res.Success || res.Error != "[FORBIDDEN] exec_envelope: policy gate not initialized (deny-by-default)" {
		t.Fatalf("expected fail-closed error, got res: %+v", res)
	}
}

func TestExecuteTool_PolicyDenied(t *testing.T) {
	r := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(&mockPolicyGate{allow: false}, sandbox.NewSandboxRouter(sandbox.NewInProcessSandbox(), nil, nil, "linux", 0), 0, "linux", nil))
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
	r, sbx := newAllowRegistry()
	_ = r.Register(minTool("boom"))
	sbx.Register("boom", func(_ context.Context, _ []byte) ([]byte, error) {
		return nil, apperr.New(apperr.CodeInternal, "sandbox kaboom")
	})

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
	r, sbx := newAllowRegistry()
	_ = r.Register(minTool("ok"))
	sbx.Register("ok", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("world"), nil
	})

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

// mockOutcomeRecorder 记录 ExecuteTool 上报的调用结果，供验证 PolicyEvolver 接线
// （2026-07-12 unwired-code-audit 补齐：ExecuteTool 结果此前从未上报给任何观察者）。
type mockOutcomeRecorder struct {
	calls []recordedOutcome
}

type recordedOutcome struct {
	toolName  string
	success   bool
	latencyMs int64
	errMsg    string
}

func (m *mockOutcomeRecorder) RecordToolOutcome(toolName string, success bool, latencyMs int64, errMsg string) {
	m.calls = append(m.calls, recordedOutcome{toolName: toolName, success: success, latencyMs: latencyMs, errMsg: errMsg})
}

func TestExecuteTool_ReportsOutcomeOnSuccess(t *testing.T) {
	r, sbx := newAllowRegistry()
	_ = r.Register(minTool("ok"))
	sbx.Register("ok", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("world"), nil
	})
	rec := &mockOutcomeRecorder{}
	r.WithOutcomeRecorder(rec)

	if _, err := r.ExecuteTool(ctxWithToken(), "ok", []byte("hello"), types.TaintNone); err != nil {
		t.Fatalf("意外 error: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("期望上报 1 次调用结果，实际 %d", len(rec.calls))
	}
	if rec.calls[0].toolName != "ok" || !rec.calls[0].success {
		t.Fatalf("上报内容不符预期: %+v", rec.calls[0])
	}
}

func TestExecuteTool_ReportsOutcomeOnSandboxError(t *testing.T) {
	r, sbx := newAllowRegistry()
	_ = r.Register(minTool("boom"))
	sbx.Register("boom", func(_ context.Context, _ []byte) ([]byte, error) {
		return nil, apperr.New(apperr.CodeInternal, "sandbox kaboom")
	})
	rec := &mockOutcomeRecorder{}
	r.WithOutcomeRecorder(rec)

	if _, err := r.ExecuteTool(ctxWithToken(), "boom", []byte("x"), types.TaintNone); err != nil {
		t.Fatalf("sandbox 错误不应作为函数 error 返回，实际: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("期望上报 1 次调用结果，实际 %d", len(rec.calls))
	}
	if rec.calls[0].success {
		t.Fatal("sandbox 报错时上报的 success 应为 false")
	}
	if rec.calls[0].errMsg == "" {
		t.Fatal("sandbox 报错时上报的 errMsg 不应为空")
	}
}

func TestExecuteTool_NilOutcomeRecorder_NoPanic(t *testing.T) {
	// 未注入 outcomeRecorder 是默认状态（生产环境改造前的行为），必须保持零开销
	// 且不 panic——回归 reportOutcome 的 nil 防御。
	r, sbx := newAllowRegistry()
	_ = r.Register(minTool("ok"))
	sbx.Register("ok", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("world"), nil
	})
	if _, err := r.ExecuteTool(ctxWithToken(), "ok", []byte("hello"), types.TaintNone); err != nil {
		t.Fatalf("意外 error: %v", err)
	}
}

func TestExecuteTool_TaintPropagation(t *testing.T) {
	r, sbx := newAllowRegistry()
	taintTool := minTool("tainted")
	taintTool.TrustTier = types.TrustLocal // Local 来源的 taint 可以保留/传播
	_ = r.Register(taintTool)
	sbx.Register("tainted", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("x"), nil
	})

	res, err := r.ExecuteTool(ctxWithToken(), "tainted", []byte("x"), types.TaintHigh)
	if err != nil {
		t.Fatalf("意外 error: %v", err)
	}
	if res.TaintLevel != types.TaintHigh {
		t.Fatalf("TaintLevel 期望 TaintHigh(%d)，实际 %d", types.TaintHigh, res.TaintLevel)
	}
}

func TestExecuteTool_ShellTool_Uses_ShellLimiter(t *testing.T) {
	sbx := sandbox.NewInProcessSandbox()
	router := sandbox.NewSandboxRouter(sbx, nil, sbx, "linux", 0) // hwTier=0 拒绝 L3
	envelope := sandbox.NewExecEnvelope(&mockPolicyGate{allow: true}, router, 0, "linux", nil)
	r := NewInMemoryToolRegistry(envelope)

	shellTool := types.Tool{
		Name:        "run-sh",
		Description: "execute shell command",
		Source:      types.ToolSkill,
		TrustTier:   types.TrustSystem,
		SideEffects: []types.SideEffect{types.SideProcessSpawn},
	}
	_ = r.Register(shellTool)
	sbx.Register("run-sh", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("ls_output"), nil
	})

	res, err := r.ExecuteTool(ctxWithToken(), "run-sh", []byte("ls"), types.TaintNone)
	if err != nil {
		t.Fatalf("shell 工具 ExecuteTool 不应 panic/error: %v", err)
	}
	expectedErr := "[FORBIDDEN] exec_envelope: route failed (isolation unavailable): [FORBIDDEN] sandbox: NativeOS required for Tier-0 CodeAct but unavailable; refusing to downgrade"
	if res.Success || res.Error != expectedErr {
		t.Fatalf("shell 工具首次调用应 Success=false，Error 期望 %q，实际为 %q", expectedErr, res.Error)
	}
}

// ─── Policy Error & Context Cancel 分支覆盖 ──────────────────────────────────

func TestExecuteTool_PolicyError(t *testing.T) {
	r := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(&mockPolicyGateWithError{}, sandbox.NewSandboxRouter(sandbox.NewInProcessSandbox(), nil, nil, "linux", 0), 0, "linux", nil))
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
	sbx := sandbox.NewInProcessSandbox()
	r := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(&mockPolicyGate{allow: true}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "linux", nil))
	_ = r.Register(minTool("ctx-tool"))
	sbx.Register("ctx-tool", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("done"), nil
	})

	ctx, cancel := context.WithCancel(ctxWithToken())
	cancel()

	result, err := r.ExecuteTool(ctx, "ctx-tool", []byte("data"), types.TaintNone)
	if err != nil {
		t.Fatalf("不期望底层 error（mock policy 忽略 ctx）: %v", err)
	}
	// mock sandbox 注册了，此时返回成功
	if !result.Success {
		t.Fatalf("cancelled ctx + allow policy 应 Success=true，Error=%q", result.Error)
	}
}
