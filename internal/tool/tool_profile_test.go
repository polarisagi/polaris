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
