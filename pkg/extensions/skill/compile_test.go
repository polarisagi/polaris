package skill

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

func TestLogicCollapseCompiler_Compile_Gate(t *testing.T) {
	// 强制 Tier 0 (单并发)
	gate := NewCompileGate(observability.Tier0)
	pub, priv, _ := ed25519.GenerateKey(nil)
	_ = pub

	c := &LogicCollapseCompiler{
		gate:       gate,
		signingKey: priv,
	}

	req := &CompileRequest{
		Trajectory: &CollapseTrajectory{
			SkillID:    "test-skill",
			TaintLevel: 0,
		},
		EvalGatePassed: true,
	}

	// 模拟并发：手动占用全部名额
	freeMB := observability.ProbeAvailableMemoryMB()
	if freeMB < compileMinFreeMemMB {
		t.Skipf("not enough memory to test compile gate: %v MB", freeMB)
	}

	for gate.TryAcquire(int64(freeMB)) {
		// keep acquiring until full
	}

	// 现在请求应该返回 ErrCompileGateBusy
	_, err := c.Compile(context.Background(), req)
	if err != ErrCompileGateBusy {
		t.Fatalf("expected ErrCompileGateBusy, got: %v", err)
	}

	// 释放 gate
	gate.Release()
}
