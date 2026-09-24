package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
)

// TestWithJITCapability_OneShot M07 §6：通过闸门后 JIT 签发、只对本次调用有效。
// 此前通用工具路径从不签发，写类工具与 trust<3 工具在执行闸门必被拒绝。
func TestWithJITCapability_OneShot(t *testing.T) {
	a := &Agent{ID: "agent-x"}
	ctx, revoke, err := a.withJITCapability(context.Background(), "write_file")
	if err != nil {
		t.Fatalf("mint failed: %v", err)
	}
	tok, ok := ctx.Value(protocol.CtxCapabilityTokenKey{}).(*token.Token)
	if !ok || tok == nil {
		t.Fatal("令牌必须经 CtxCapabilityTokenKey 注入本次调用的 ctx")
	}
	if err := action.GetTokenManager().Verify(tok); err != nil {
		t.Fatalf("调用期间令牌应可被执行闸门验签通过: %v", err)
	}
	revoke()
	if err := action.GetTokenManager().Verify(tok); err == nil {
		t.Fatal("调用结束撤销后令牌不得再被复用")
	}
}
