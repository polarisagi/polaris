package tool

import (
	"testing"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
)

func TestToolRegistryTrustTier(t *testing.T) {
	policyGate := policy.NewGate(nil)
	router := sandbox.NewSandboxRouter(nil, nil, nil, "linux", 3)
	env := sandbox.NewExecEnvelope(policyGate, router, 3, "linux", nil)

	reg := NewInMemoryToolRegistry(env)
	meta, _ := GetBuiltinToolMeta("tool_search")
	reg.Register(meta)

	tool, _ := reg.Lookup("tool_search")
	t.Logf("Lookup tool TrustTier: %d", tool.TrustTier)
}
