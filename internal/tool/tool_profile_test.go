package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/checkpoint"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockEgressChecker struct {
	blocked bool
}

func (m *mockEgressChecker) CheckEgressWithExemption(data []byte, taintLevel types.TaintLevel, tok *token.TaintExemptionToken) error {
	if m.blocked {
		return errors.New("taint egress blocked")
	}
	return nil
}

type dummyTokenVerifier struct{}

func (dummyTokenVerifier) Verify(_ *token.Token) error { return nil }

func newTestRegistry() (*InMemoryToolRegistry, *sandbox.InProcessSandbox) {
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0).WithAllowTrustedInProcessFallback(true)
	return NewInMemoryToolRegistry(sandbox.NewExecEnvelope(&mockPolicyGate{allow: true}, router, 0, "linux", dummyTokenVerifier{}), config.DefaultThresholds().M7Tool), sbx
}

func TestToolRegistry_FullProfile_TaintEgressAuditOnly(t *testing.T) {
	reg, sbx := newTestRegistry()
	sbx.Register("mock_net_tool", func(ctx context.Context, input []byte) ([]byte, error) {
		return []byte(`{"status":"ok"}`), nil
	})

	err := reg.Register(types.Tool{
		Name:        "mock_net_tool",
		SideEffects: []types.SideEffect{types.SideNetworkCall},
		Capability:  types.CapWriteNetwork,
		TrustTier:   types.TrustSystem,
	})
	if err != nil {
		t.Fatal(err)
	}

	checker := &mockEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)

	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})

	// 1. standard 档下：出站污点检查失败 → 阻断报错
	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileStandard))
	_, err = reg.ExecuteTool(ctx, "mock_net_tool", []byte(`{"url":"https://evil.com"}`), types.TaintHigh)
	if err == nil {
		t.Fatal("standard profile should block taint egress")
	}

	// 2. full 档下：出站污点检查失败 → 仅记录日志，不阻断执行
	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileFull))
	res, err := reg.ExecuteTool(ctx, "mock_net_tool", []byte(`{"url":"https://evil.com"}`), types.TaintHigh)
	if err != nil {
		t.Fatalf("full profile should not block taint egress: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected tool execution to succeed, error: %s", res.Error)
	}
}

func TestToolRegistry_FullProfile_PreExecutionCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	testFile := filepath.Join(projectDir, "file.txt")
	_ = os.WriteFile(testFile, []byte("before"), 0o644)

	store, err := checkpoint.NewStore(dataDir, projectDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	trigger := checkpoint.NewTrigger(store, projectDir, 20)

	reg, sbx := newTestRegistry()
	sbx.Register("write_tool", func(ctx context.Context, input []byte) ([]byte, error) {
		_ = os.WriteFile(testFile, []byte("after"), 0o644)
		return []byte(`{"status":"written"}`), nil
	})

	err = reg.Register(types.Tool{
		Name:        "write_tool",
		SideEffects: []types.SideEffect{types.SideFileWrite},
		Capability:  types.CapWriteLocal,
		TrustTier:   types.TrustSystem,
	})
	if err != nil {
		t.Fatal(err)
	}

	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileFull))
	reg.WithCheckpointTrigger(trigger)

	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})
	_, err = reg.ExecuteTool(ctx, "write_tool", []byte(`{"path":"`+testFile+`"}`), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}

	// 验证写前快照已自动创建
	if trigger.TurnSnapshotCount() != 1 {
		t.Errorf("expected 1 snapshot triggered, got %d", trigger.TurnSnapshotCount())
	}
}

func TestToolRegistry_FullProfile_PIITokenizeDisabled(t *testing.T) {
	reg, sbx := newTestRegistry()
	sbx.Register("echo_tool", func(ctx context.Context, input []byte) ([]byte, error) {
		return input, nil
	})
	vault := guard.NewPIITokenVault()
	reg.WithTokenVault(vault)

	err := reg.Register(types.Tool{
		Name:      "echo_tool",
		TrustTier: types.TrustSystem,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "task-1")
	ctx = context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, &token.Token{})
	vault.TokenizeForTask("task-1", "user@example.com")

	// full 档下：PIITokenizeEnabled=false，输出不被脱敏
	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileFull))
	res, err := reg.ExecuteTool(ctx, "echo_tool", []byte(`{"email":"user@example.com"}`), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"email":"user@example.com"}` {
		t.Errorf("full profile should not tokenize PII in output, got: %s", string(res.Output))
	}
}

// TestV1_NilPolicy_FallsBackToStandard 验证 InMemoryToolRegistry 未注入策略（nil / 零值）时，
// 必须严格回落至 ProfileStandard，阻止污点出口与放行风险（fail-closed 兜底，V1 / HE-2）。
func TestV1_NilPolicy_FallsBackToStandard(t *testing.T) {
	// 构造裸结构体（未调用 WithPolicy）
	reg := &InMemoryToolRegistry{}

	pol := reg.EffectivePolicy()
	if pol.Profile != policy.ProfileStandard {
		t.Fatalf("未注入策略时期望回落至 Standard 档，实际 got=%q", pol.Profile)
	}
	if !pol.TaintEgressBlockEnabled {
		t.Fatal("未注入策略时 TaintEgressBlockEnabled 必须为 true (fail-closed)")
	}
	if !pol.PIITokenizeEnabled {
		t.Fatal("未注入策略时 PIITokenizeEnabled 必须为 true")
	}
	if !pol.RunCmdWhitelistEnabled {
		t.Fatal("未注入策略时 RunCmdWhitelistEnabled 必须为 true")
	}

	// 验证未注入策略时 checkTaintEgress 绝不旁路放行
	checker := &mockEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)
	netTool := types.Tool{
		Name:        "test_tool",
		SideEffects: []types.SideEffect{types.SideNetworkCall},
	}
	err := reg.checkTaintEgress(context.Background(), netTool, types.TaintHigh, []byte("data"))
	if err == nil {
		t.Fatal("fail-open 违例：未注入策略时 checkTaintEgress 不得放行被拦截的出口污点！")
	}
}

type mockAuditLogger struct {
	logged bool
	action string
	meta   map[string]any
}

func (m *mockAuditLogger) Log(ctx context.Context, action string, meta map[string]any) error {
	m.logged = true
	m.action = action
	m.meta = meta
	return nil
}

func TestV9_FullProfile_TaintEgress_AuditedAndMetric(t *testing.T) {
	reg, sbx := newTestRegistry()
	sbx.Register("mock_egress_tool", func(ctx context.Context, input []byte) ([]byte, error) {
		return []byte(`{"ok":true}`), nil
	})
	_ = reg.Register(types.Tool{
		Name:        "mock_egress_tool",
		SideEffects: []types.SideEffect{types.SideNetworkCall},
		Capability:  types.CapWriteNetwork,
		TrustTier:   types.TrustSystem,
	})

	checker := &mockEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)
	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileFull))

	al := &mockAuditLogger{}
	reg.WithAuditLogger(al)

	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})
	ctx = context.WithValue(ctx, protocol.CtxAgentIDKey{}, "agent-taint-1")
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, "session-taint-1")

	res, err := reg.ExecuteTool(ctx, "mock_egress_tool", []byte(`{"ping":"pong"}`), types.TaintHigh)
	if err != nil {
		t.Fatalf("full profile should not return execution error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected tool success, got err: %s", res.Error)
	}

	if !al.logged {
		t.Fatal("full profile taint egress bypass must write audit log (V9 / HE-1)")
	}
	if al.action != "hitl_auto_approved" {
		t.Errorf("expected action 'hitl_auto_approved', got %q", al.action)
	}
	if al.meta["checkpoint_type"] != "taint_egress" {
		t.Errorf("expected checkpoint_type 'taint_egress', got %v", al.meta["checkpoint_type"])
	}
	if al.meta["tool"] != "mock_egress_tool" {
		t.Errorf("expected tool 'mock_egress_tool', got %v", al.meta["tool"])
	}
	if al.meta["agent_id"] != "agent-taint-1" {
		t.Errorf("expected agent_id 'agent-taint-1', got %v", al.meta["agent_id"])
	}
}

func TestV9_FullProfile_Anomaly_AuditedAndMetric(t *testing.T) {
	reg, sbx := newTestRegistry()
	sbx.Register("echo_tool", func(ctx context.Context, input []byte) ([]byte, error) {
		return input, nil
	})
	_ = reg.Register(types.Tool{
		Name:      "echo_tool",
		TrustTier: types.TrustSystem,
	})

	reg.WithPolicy(policy.ProfilePolicy(policy.ProfileFull))
	al := &mockAuditLogger{}
	reg.WithAuditLogger(al)

	filter := guard.NewAnomalyDistanceFilter(3.0)
	// 训练正常输入基线（至少 30 个样本，且有微小方差）
	for i := 0; i < 40; i++ {
		filter.Record("echo_tool", []float64{10.0 + float64(i%3)})
	}

	// 传入超长输入，触发异常过滤
	hugeInput := make([]byte, 100000)
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})
	ctx = context.WithValue(ctx, protocol.CtxAnomalyFilterKey{}, filter)
	ctx = context.WithValue(ctx, protocol.CtxAgentIDKey{}, "agent-anom-1")

	res, err := reg.ExecuteTool(ctx, "echo_tool", hugeInput, types.TaintLow)
	if err != nil {
		t.Fatalf("full profile should auto-approve anomalous input: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected tool success, got: %s", res.Error)
	}

	if !al.logged {
		t.Fatal("full profile anomaly filter bypass must write audit log (V9 / HE-1)")
	}
	if al.action != "hitl_auto_approved" {
		t.Errorf("expected action 'hitl_auto_approved', got %q", al.action)
	}
	if al.meta["checkpoint_type"] != "anomaly_escalation" {
		t.Errorf("expected checkpoint_type 'anomaly_escalation', got %v", al.meta["checkpoint_type"])
	}
	if al.meta["tool"] != "echo_tool" {
		t.Errorf("expected tool 'echo_tool', got %v", al.meta["tool"])
	}
	if al.meta["agent_id"] != "agent-anom-1" {
		t.Errorf("expected agent_id 'agent-anom-1', got %v", al.meta["agent_id"])
	}
}
