package lifecycle

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestAppBindings_ResolveBindAndPreserve(t *testing.T) {
	extRepo := newTestExtRepo(t)
	ctx := context.Background()
	for _, row := range []types.MCPServerRow{{ID: "mcp_github", Name: "github", Transport: "stdio", Command: "gh-mcp"},
		{ID: "mcp_linear", Name: "linear", Transport: "stdio", Command: "linear-mcp"}} {
		if err := extRepo.UpsertMCPServer(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(t.TempDir(), "work")
	writeTestFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{"name":"work","apps":"./.app.json"}`)
	writeTestFile(t, filepath.Join(root, ".app.json"), `{"apps":{"gh":{"id":"github"},"slack":{"id":"connector_abc"}}}`)
	writeTestFile(t, filepath.Join(root, "skills", "s", "SKILL.md"), "---\nname: s\ndescription: d\n---\nbody")
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	install := func() {
		if _, err := inst.Install(ctx, InstallReq{InstID: "ext_w", LocalPath: root}); err != nil {
			t.Fatal(err)
		}
	}
	install()
	got := appsByAlias(t, extRepo)
	if got["gh"].Status != AppBound || got["gh"].BoundServerID != "mcp_github" || got["slack"].Status != AppUnbound {
		t.Fatalf("resolve: %+v", got)
	}
	if _, err := BindApp(ctx, extRepo, "pl_w", "slack", "missing"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("binding to an unknown connector must fail: %v", err)
	}
	if _, err := BindApp(ctx, extRepo, "pl_w", "slack", "mcp_linear"); err != nil {
		t.Fatal(err)
	}
	install() // 升级重装：手动绑定保留
	if got := appsByAlias(t, extRepo); got["slack"].BoundServerID != "mcp_linear" {
		t.Fatalf("manual binding must survive reinstall: %+v", got)
	}
	if b, err := BindApp(ctx, extRepo, "pl_w", "slack", ""); err != nil || b.Status != AppUnbound {
		t.Fatalf("unbind: %+v %v", b, err)
	}
}

func appsByAlias(t *testing.T, extRepo *testExtRepo) map[string]types.PluginAppBinding {
	t.Helper()
	bindings, err := extRepo.ListPluginAppBindings(context.Background(), "pl_w")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]types.PluginAppBinding{}
	for _, b := range bindings {
		out[b.Alias] = b
	}
	return out
}
