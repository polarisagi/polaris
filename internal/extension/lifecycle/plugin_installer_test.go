package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type recordingSkillRegistry struct {
	mu    sync.Mutex
	metas map[string]types.SkillMeta
}

func (r *recordingSkillRegistry) Register(_ context.Context, meta types.SkillMeta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.metas == nil {
		r.metas = map[string]types.SkillMeta{}
	}
	r.metas[meta.Name] = meta
	return nil
}
func (r *recordingSkillRegistry) Get(context.Context, string, string) (*types.SkillMeta, error) {
	return nil, nil
}
func (r *recordingSkillRegistry) List(context.Context, types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}
func (r *recordingSkillRegistry) Deprecate(context.Context, string, string, string) error { return nil }

type recordingConnector struct {
	mu      sync.Mutex
	started []string
	removed []string
}

func (c *recordingConnector) StartFromDB(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, id)
	return nil
}
func (c *recordingConnector) GetClient(string) protocol.MCPClient { return nil }
func (c *recordingConnector) Remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = append(c.removed, id)
}

type allowAllGate struct{ deny map[string]bool }

func (g allowAllGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}

func (g allowAllGate) Review(_ context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: !g.deny[req.Resource]}, nil
}

func writeClaudePlugin(t *testing.T, withExtraSkill bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "deploy-tools")
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"deploy-tools","version":"1.2.0","description":"d","author":{"name":"Acme"},
		  "mcpServers":{"api":{"command":"node","args":["${CLAUDE_PLUGIN_ROOT}/server.js"]}}}`)
	writeTestFile(t, filepath.Join(root, "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: Deploy\n---\nsteps")
	writeTestFile(t, filepath.Join(root, "commands", "frontend", "build.md"), "---\ndescription: Build\n---\nbuild")
	writeTestFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"remote":{"type":"http","url":"https://x.example/mcp","headers":{"Authorization":"Bearer ${user_config.tok}"}}}}`)
	if withExtraSkill {
		writeTestFile(t, filepath.Join(root, "skills", "rollback", "SKILL.md"), "---\nname: rollback\ndescription: Rollback\n---\n")
	}
	return root
}

func TestPluginInstaller_ClaudePluginEndToEnd(t *testing.T) {
	extRepo := newTestExtRepo(t)
	reg := &recordingSkillRegistry{}
	conn := &recordingConnector{}
	inst := NewPluginInstaller(extRepo, conn, reg).WithPolicyGate(allowAllGate{})
	root := writeClaudePlugin(t, false)

	res, err := inst.Install(context.Background(), InstallReq{InstID: "ext_abc", Name: "entry", LocalPath: root, TrustTier: 2})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.RuntimeID != "pl_abc" || res.Dir != root {
		t.Fatalf("result: %+v", res)
	}
	if _, ok := reg.metas["skill:deploy-tools__deploy"]; !ok {
		t.Fatalf("skill not registered: %v", keys(reg.metas))
	}
	if _, ok := reg.metas["skill:deploy-tools__frontend__build"]; !ok {
		t.Fatalf("command not registered as skill: %v", keys(reg.metas))
	}
	servers, _ := extRepo.ListMCPServers(context.Background())
	byID := map[string]types.MCPServerRow{}
	for _, s := range servers {
		byID[s.ID] = s
	}
	api, remote := byID["plugin_pl_abc_api"], byID["plugin_pl_abc_remote"]
	if api.PluginID != "pl_abc" || api.Args != `["${CLAUDE_PLUGIN_ROOT}/server.js"]` {
		t.Fatalf("stdio row must keep raw template: %+v", api)
	}
	if remote.Transport != string(protocol.MCPStreamableHTTP) || remote.Headers != `{"Authorization":"Bearer ${user_config.tok}"}` {
		t.Fatalf("remote row: %+v", remote)
	}
	path, err := extRepo.GetPluginInstallPath(context.Background(), "pl_abc")
	if err != nil || path != root {
		t.Fatalf("plugin row install path: %q %v", path, err)
	}
	waitStarted(t, conn, 2)
}

func TestPluginInstaller_ReinstallRemovesStaleComponents(t *testing.T) {
	extRepo := newTestExtRepo(t)
	conn := &recordingConnector{}
	inst := NewPluginInstaller(extRepo, conn, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	root := writeClaudePlugin(t, true)
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_abc", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
	// 新版本删除了 .mcp.json 中的 remote 服务器。
	writeTestFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{}}`)
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_abc", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
	servers, _ := extRepo.ListMCPServers(context.Background())
	for _, s := range servers {
		if s.ID == "plugin_pl_abc_remote" {
			t.Fatalf("stale server survived reinstall")
		}
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.removed) < 2 {
		t.Fatalf("running servers of previous version must be stopped, removed=%v", conn.removed)
	}
}

func TestPluginInstaller_PolicyDeniedServerIsRecordedNotStarted(t *testing.T) {
	extRepo := newTestExtRepo(t)
	conn := &recordingConnector{}
	gate := allowAllGate{deny: map[string]bool{"plugin_pl_abc_api": true}}
	inst := NewPluginInstaller(extRepo, conn, &recordingSkillRegistry{}).WithPolicyGate(gate)
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_abc", LocalPath: writeClaudePlugin(t, false)}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, conn, 1)
	conn.mu.Lock()
	started := append([]string(nil), conn.started...)
	conn.mu.Unlock()
	if len(started) != 1 || started[0] != "plugin_pl_abc_remote" {
		t.Fatalf("denied server must not start: %v", started)
	}
	manifest := pluginManifest(t, extRepo)
	if !hasDiagRule(manifest.Diagnostics, "polaris.mcp.policy") {
		t.Fatalf("policy denial must be recorded in manifest diagnostics: %v", manifest.Diagnostics)
	}
}

func TestPluginInstaller_NoPolicyGateFailsClosed(t *testing.T) {
	extRepo := newTestExtRepo(t)
	conn := &recordingConnector{}
	inst := NewPluginInstaller(extRepo, conn, &recordingSkillRegistry{})
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_abc", LocalPath: writeClaudePlugin(t, false)}); err != nil {
		t.Fatal(err)
	}
	servers, _ := extRepo.ListMCPServers(context.Background())
	if len(servers) != 0 {
		t.Fatalf("without policy gate no bundle MCP may be persisted, got %d", len(servers))
	}
}

func pluginManifest(t *testing.T, extRepo *testExtRepo) pluginspec.Plugin {
	t.Helper()
	var raw string
	if err := extRepo.db.QueryRowContext(context.Background(), "SELECT manifest FROM plugins WHERE id='pl_abc'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var p pluginspec.Plugin
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func hasDiagRule(ds []pluginspec.Diagnostic, rule string) bool {
	for _, d := range ds {
		if d.Rule == rule {
			return true
		}
	}
	return false
}

func waitStarted(t *testing.T, conn *recordingConnector, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn.mu.Lock()
		got := len(conn.started)
		conn.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d servers started", n)
}

func keys(m map[string]types.SkillMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
