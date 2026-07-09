package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockPolicyGate 允许所有调用通过（PII 隔离测试关注令牌命名空间，不关注策略决策）。
type mockPolicyGate struct{ allow bool }

func (m *mockPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return m.allow, nil
}

func (m *mockPolicyGate) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: m.allow}, nil
}

func ctxWithCapabilityToken(base context.Context) context.Context {
	tok, _ := action.GetTokenManager().Mint("test-agent", []token.CapabilityType{token.CapProcess}, 1, 5*time.Minute, 0)
	return context.WithValue(base, protocol.CtxCapabilityToken{}, tok)
}

func TestTokenizeMessagesForLLM(t *testing.T) {
	detector := guard.NewPIIDetector()
	vault := guard.NewPIITokenVault()
	a := &Agent{
		piiDetector: detector,
		tokenVault:  vault,
	}

	taskID := "task-123"
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, taskID)

	messages := []types.Message{
		{Role: "user", Content: "My email is test@example.com"},
	}

	outMsgs, err := a.tokenizeMessagesForLLM(ctx, messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(outMsgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(outMsgs))
	}

	content := outMsgs[0].Content
	if strings.Contains(content, "test@example.com") {
		t.Errorf("PII was not redacted: %s", content)
	}

	if !strings.Contains(content, "⟦PII:") {
		t.Errorf("expected PII token in content: %s", content)
	}

	start := strings.Index(content, "⟦PII:")
	end := strings.Index(content, "⟧") + len("⟧")
	if start == -1 || end < len("⟧") {
		t.Fatalf("could not extract token from %s", content)
	}
	tok := content[start:end]

	realValue, err := vault.ResolveForTask(taskID, tok)
	if err != nil {
		t.Fatalf("failed to resolve token: %v", err)
	}
	if realValue != "test@example.com" {
		t.Errorf("expected 'test@example.com', got '%s'", realValue)
	}
}

// TestWithTaskScopeCtx_InjectsSessionID 验证 executeEffect 入口注入的命名空间键
// 使用的是 a.sCtx.SessionID，而不是 a.sCtx.TaskID——这是本次修复的核心回归点：
// 此前生产路径从未做过这个注入，tokenizeMessagesForLLM 收到的 ctx 恒不带
// CtxTaskIDKey，所有请求的 PII token 都退化写入共享的空 taskID 命名空间。
func TestWithTaskScopeCtx_InjectsSessionID(t *testing.T) {
	a := &Agent{
		sCtx: &fsm.StateContext{
			SessionID: "session-abc",
			TaskID:    "blackboard-task-xyz", // 故意设为不同值，验证不会被误用
		},
	}

	ctx := a.withTaskScopeCtx(context.Background())

	got, ok := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	if !ok {
		t.Fatal("expected CtxTaskIDKey to be set in ctx")
	}
	if got != "session-abc" {
		t.Errorf("expected ctx taskID scope to be SessionID 'session-abc', got %q (TaskID leaking in would be 'blackboard-task-xyz')", got)
	}
}

// TestWithTaskScopeCtx_EmptySessionID_NoInjection 验证 SessionID 为空时不注入，
// 不覆盖调用方已有的 ctx 值（例如脱离完整 Agent 生命周期的调用场景）。
func TestWithTaskScopeCtx_EmptySessionID_NoInjection(t *testing.T) {
	a := &Agent{sCtx: &fsm.StateContext{}}

	ctx := a.withTaskScopeCtx(context.Background())

	if _, ok := ctx.Value(protocol.CtxTaskIDKey{}).(string); ok {
		t.Fatal("expected no CtxTaskIDKey injection when SessionID is empty")
	}
}

// TestPIIOpaqueTokenIntegration 端到端验证 OpaqueToken 语义闭环：
//  1. Agent 侧用 SessionID 命名空间对消息做 tokenize（模拟 executeEffect 注入后的 ctx）；
//  2. 模拟 LLM 原样回传令牌；
//  3. 真实调用 InMemoryToolRegistry.ExecuteTool（而非绕过直接调 RestoreForTask），
//     验证 ExecuteTool 内部从 ctx 提取的 taskID 与 tokenize 时使用的 SessionID
//     一致，从而工具收到的是还原后的明文，而不是原样透传的令牌字面量。
func TestPIIOpaqueTokenIntegration(t *testing.T) {
	detector := guard.NewPIIDetector()
	vault := guard.NewPIITokenVault()
	a := &Agent{
		piiDetector: detector,
		tokenVault:  vault,
		sCtx:        &fsm.StateContext{SessionID: "session-integration"},
	}

	// 1. Agent Tokenize：ctx 经 withTaskScopeCtx 注入 SessionID 命名空间
	// （复现 executeEffect 的真实调用顺序，不手工在测试里另起 taskID）。
	agentCtx := a.withTaskScopeCtx(context.Background())
	inputMsgs := []types.Message{
		{Role: "user", Content: "Please contact me at 13812345678 regarding the issue."},
	}
	outMsgs, err := a.tokenizeMessagesForLLM(agentCtx, inputMsgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tokenizedContent := outMsgs[0].Content
	if !strings.Contains(tokenizedContent, "⟦PII:") {
		t.Fatalf("expected tokenized content, got %s", tokenizedContent)
	}
	if strings.Contains(tokenizedContent, "13812345678") {
		t.Fatalf("PII leaked into content sent to LLM: %s", tokenizedContent)
	}

	// 2. 模拟 LLM 原样回传令牌（模型不理解 token 语义，原样透传）。
	llmResponse := tokenizedContent

	// 3. 真实调用 ExecuteTool：构造一个最小可用的 ExecEnvelope + InProcessSandbox，
	// 注册一个 echo 工具捕获它实际收到的 input，验证已被还原为明文。
	sbx := sandbox.NewInProcessSandbox()
	router := sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0)
	envelope := sandbox.NewExecEnvelope(&mockPolicyGate{allow: true}, router, 0, "linux", nil)
	registry := tool.NewInMemoryToolRegistry(envelope).WithTokenVault(vault)

	if err := registry.Register(types.Tool{
		Name:      "echo_tool",
		Source:    types.ToolBuiltin,
		TrustTier: types.TrustSystem,
	}); err != nil {
		t.Fatalf("failed to register tool: %v", err)
	}

	var capturedInput string
	sbx.Register("echo_tool", func(_ context.Context, input []byte) ([]byte, error) {
		capturedInput = string(input)
		return input, nil
	})

	// 关键：ExecuteTool 用的 ctx 必须和 tokenize 阶段用的是同一个 taskID 命名空间
	// （agentCtx，携带 SessionID）——这正是本次修复要保证的一致性，也是此前
	// bug 所在（生产路径两端从未真正对齐过同一个 ctx 值）。
	toolCtx := ctxWithCapabilityToken(agentCtx)
	res, err := registry.ExecuteTool(toolCtx, "echo_tool", []byte(llmResponse), types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("ExecuteTool expected success, got Error=%q", res.Error)
	}

	if !strings.Contains(capturedInput, "13812345678") {
		t.Errorf("expected tool to receive restored plaintext phone number, got %q", capturedInput)
	}
	if strings.Contains(capturedInput, "⟦PII:") {
		t.Errorf("expected tool input to have PII token resolved, still contains token: %q", capturedInput)
	}
}

// TestPIITokenVault_NoCrossTaskLeak 验证任务级隔离的反面场景：用错误的（其他）
// taskID 试图还原令牌必须 fail-closed 拒绝，不能静默从其他命名空间读到值
// ——这是此前"回退查找 v.tokens[”]"实现被移除的原因所在。
func TestPIITokenVault_NoCrossTaskLeak(t *testing.T) {
	vault := guard.NewPIITokenVault()
	tok := vault.TokenizeForTask("session-A", "secret-for-A@example.com")

	if _, err := vault.ResolveForTask("session-B", tok); err == nil {
		t.Fatal("expected ResolveForTask with a different taskID to fail-closed, got nil error")
	}

	// 正确的 taskID 必须能正常还原。
	val, err := vault.ResolveForTask("session-A", tok)
	if err != nil {
		t.Fatalf("expected resolve with correct taskID to succeed, got error: %v", err)
	}
	if val != "secret-for-A@example.com" {
		t.Errorf("expected restored value, got %q", val)
	}
}

// TestPIITokenVault_ClearTask_OnlyClearsOwnNamespace 验证 ClearTask 只清理指定
// taskID 的命名空间，不影响其他并发任务的令牌（handleTerminalState 每次终态触发
// 时都会调用 ClearTask(a.sCtx.SessionID)，必须不误伤同一进程内其他并发会话）。
func TestPIITokenVault_ClearTask_OnlyClearsOwnNamespace(t *testing.T) {
	vault := guard.NewPIITokenVault()
	tokA := vault.TokenizeForTask("session-A", "a@example.com")
	tokB := vault.TokenizeForTask("session-B", "b@example.com")

	vault.ClearTask("session-A")

	if _, err := vault.ResolveForTask("session-A", tokA); err == nil {
		t.Fatal("expected session-A token to be cleared")
	}
	if val, err := vault.ResolveForTask("session-B", tokB); err != nil || val != "b@example.com" {
		t.Fatalf("expected session-B token to survive session-A's ClearTask, got val=%q err=%v", val, err)
	}
}
