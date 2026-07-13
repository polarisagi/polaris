package tool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockTaintEgressChecker 记录调用参数，便于断言 exemption token 是否被正确传入。
type mockTaintEgressChecker struct {
	lastData       []byte
	lastTaintLevel types.TaintLevel
	lastTok        *token.TaintExemptionToken
	blocked        bool
}

func (m *mockTaintEgressChecker) CheckEgressWithExemption(data []byte, taintLevel types.TaintLevel, tok *token.TaintExemptionToken) error {
	m.lastData = data
	m.lastTaintLevel = taintLevel
	m.lastTok = tok
	if m.blocked && (tok == nil || !tok.Valid(data)) {
		return apperr.New(apperr.CodeForbidden, "blocked")
	}
	return nil
}

func networkTool(name string) types.Tool {
	t := minTool(name)
	t.SideEffects = []types.SideEffect{types.SideNetworkCall}
	return t
}

func TestCheckTaintEgress_SkipsWhenCheckerNotInjected(t *testing.T) {
	reg, _ := newAllowRegistry()
	err := reg.checkTaintEgress(context.Background(), networkTool("fetch_url"), types.TaintHigh, []byte("data"))
	if err != nil {
		t.Fatalf("expected nil error when taintEgressChecker not injected, got %v", err)
	}
}

func TestCheckTaintEgress_SkipsForNonNetworkTool(t *testing.T) {
	reg, _ := newAllowRegistry()
	checker := &mockTaintEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)

	err := reg.checkTaintEgress(context.Background(), minTool("read_file"), types.TaintHigh, []byte("data"))
	if err != nil {
		t.Fatalf("expected nil error for non-network tool, got %v", err)
	}
	if checker.lastData != nil {
		t.Error("checker should not have been invoked for a non-network tool")
	}
}

func TestCheckTaintEgress_SkipsBelowTaintMedium(t *testing.T) {
	reg, _ := newAllowRegistry()
	checker := &mockTaintEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)

	err := reg.checkTaintEgress(context.Background(), networkTool("fetch_url"), types.TaintLow, []byte("data"))
	if err != nil {
		t.Fatalf("expected nil error below TaintMedium, got %v", err)
	}
}

func TestCheckTaintEgress_BlocksAtTaintMediumWithoutExemption(t *testing.T) {
	reg, _ := newAllowRegistry()
	checker := &mockTaintEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)

	err := reg.checkTaintEgress(context.Background(), networkTool("fetch_url"), types.TaintMedium, []byte("secret"))
	if err == nil {
		t.Fatal("expected blocking error at TaintMedium with no exemption vault/token")
	}
}

// TestCheckTaintEgress_AllowsWithValidExemptionToken 验证 ExemptionVault 里
// 存有匹配 AgentID + 内容哈希一致的令牌时，出口检查放行——端到端覆盖
// HITL 铸造→存储→下一次工具调用查询 这条此前完全不存在的转义路径。
func TestCheckTaintEgress_AllowsWithValidExemptionToken(t *testing.T) {
	reg, _ := newAllowRegistry()
	checker := &mockTaintEgressChecker{blocked: true}
	reg.WithTaintEgressChecker(checker)

	vault := token.NewExemptionVault()
	data := []byte("secret payload")
	tok := token.NewTaintExemptionToken(data, time.Minute, "reviewer-1")
	vault.Store("agent-42", tok)
	reg.WithExemptionVault(vault)

	ctx := context.WithValue(context.Background(), protocol.CtxAgentIDKey{}, "agent-42")
	err := reg.checkTaintEgress(ctx, networkTool("fetch_url"), types.TaintMedium, data)
	if err != nil {
		t.Fatalf("expected exemption token to allow egress, got %v", err)
	}
	if checker.lastTok == nil {
		t.Error("expected exemption token to be passed to the checker")
	}
}

// TestCheckTaintEgress_WrappedErrorPreservesErrorsIs 验证被拦截时返回的错误
// 经 apperr.Wrap 后仍可通过 errors.Is 识别底层 policy.ErrTaintBlockedEgress
// 类语义——本测试用一个满足同样 Unwrap 约定的哨兵错误模拟 policy 包的行为，
// 不直接依赖 internal/security/policy（避免循环 import 风险，checkTaintEgress
// 本身对 checker 返回值是完全不透明的透传+包装）。
func TestCheckTaintEgress_WrappedErrorPreservesErrorsIs(t *testing.T) {
	sentinel := apperr.New(apperr.CodeForbidden, "sentinel: egress blocked")
	reg, _ := newAllowRegistry()
	reg.WithTaintEgressChecker(&sentinelReturningChecker{err: sentinel})

	err := reg.checkTaintEgress(context.Background(), networkTool("fetch_url"), types.TaintMedium, []byte("x"))
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped error chain to preserve errors.Is match on sentinel, got %v", err)
	}
}

type sentinelReturningChecker struct{ err error }

func (s *sentinelReturningChecker) CheckEgressWithExemption(_ []byte, _ types.TaintLevel, _ *token.TaintExemptionToken) error {
	return s.err
}
