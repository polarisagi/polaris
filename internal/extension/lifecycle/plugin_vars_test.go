package lifecycle

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/types"
)

// 插件子 MCP 声明的 startup_timeout_sec 经 manifest 快照在重启后仍可解析（无需 DDL 列）。
func TestPluginVarsResolver_StartupTimeoutFromManifest(t *testing.T) {
	extRepo := newTestExtRepo(t)
	plug := pluginspec.Plugin{Name: "p", MCPServers: []pluginspec.MCPServer{
		{Name: "slow", Extra: map[string]json.RawMessage{"startup_timeout_sec": json.RawMessage(`600`)}},
		{Name: "plain"},
	}}
	snap, _ := json.Marshal(plug)
	if err := extRepo.UpsertPlugin(context.Background(), types.PluginRow{ID: "pl_x", Name: "p", Version: "1", InstallPath: t.TempDir(), Manifest: string(snap), MCPPolicy: "{}"}); err != nil {
		t.Fatal(err)
	}
	r := NewPluginVarsResolver(extRepo, t.TempDir(), nil)
	vars, err := r.ResolvePluginVars(context.Background(), "pl_x", "plugin_pl_x_slow")
	if err != nil || vars.StartupTimeout != 600*time.Second {
		t.Fatalf("slow: %v %v", vars.StartupTimeout, err)
	}
	vars, err = r.ResolvePluginVars(context.Background(), "pl_x", "plugin_pl_x_plain")
	if err != nil || vars.StartupTimeout != 0 {
		t.Fatalf("plain: %v %v", vars.StartupTimeout, err)
	}
}
