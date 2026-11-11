package sandbox

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	isandbox "github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
)

func TestBuildSandboxContext_MergesExplicitAndDefaultDenyPaths(t *testing.T) {
	customRead := "/trusted/source"
	customWrite := "/trusted/readonly"
	ctx := buildSandboxContext(isandbox.CmdRunnerCfg{
		CallerType: "builtin", Command: "true", WorkDir: "/scratch", AllowedPaths: []string{"/scratch"},
		DenyPaths: []string{customRead}, DenyWritePaths: []string{customWrite}, NetworkBlock: true,
	})
	defaultRead, defaultWrite := policy.SandboxDenyPaths()
	if ctx.NetworkPolicy != protocol.NetPolicyDeny {
		t.Fatalf("network deny was not preserved: %s", ctx.NetworkPolicy)
	}
	if !hasPath(ctx.DenyPaths, customRead) || !hasPath(ctx.DenyWritePaths, customWrite) {
		t.Fatalf("explicit denies missing: %+v", ctx)
	}
	for _, path := range defaultRead {
		if !hasPath(ctx.DenyPaths, path) {
			t.Fatalf("default deny path %q was dropped", path)
		}
	}
	for _, path := range defaultWrite {
		if !hasPath(ctx.DenyWritePaths, path) {
			t.Fatalf("default deny-write path %q was dropped", path)
		}
	}
}

func hasPath(paths []string, target string) bool {
	for _, path := range paths {
		if path == target {
			return true
		}
	}
	return false
}
