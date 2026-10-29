package chat

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
)

type stubMCPMgr struct{ servers []protocol.MCPServerInfo }

func (m *stubMCPMgr) ListServers() []protocol.MCPServerInfo { return m.servers }
func (m *stubMCPMgr) IsPluginConnected(string) bool         { return false }

func newPluginDB(t *testing.T, rows ...[4]string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE plugins (id TEXT, name TEXT, display_name TEXT, mcp_policy TEXT, enabled INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO plugins VALUES (?,?,?,?,1)`, r[0], r[1], r[2], r[3]); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// 稳定层的扩展名称清单不得含连接状态，且与 MCP 遍历序/DB 行序无关；状态进入易变层。
func TestExtensionSummary_StableNamesVolatileStatus(t *testing.T) {
	// 故意乱序插入，且 mcp_policy 引用的子 MCP 决定 ✓/✗。
	db := newPluginDB(t,
		[4]string{"p2", "zeta", "", `{"srv":{"enabled":true}}`},
		[4]string{"p1", "alpha", "Alpha", `{"srv":{"enabled":true}}`},
	)
	servers := func(connected bool) []protocol.MCPServerInfo {
		return []protocol.MCPServerInfo{
			{ID: "m_b", Name: "beta", Connected: connected},
			{ID: "plugin_p1_srv", Name: "x", Connected: connected},
			{ID: "m_a", Name: "amber", Connected: !connected},
			{ID: "plugin_p2_srv", Name: "y", Connected: connected},
		}
	}
	svc := &PromptAssemblyService{DB: db, MCPMgr: &stubMCPMgr{servers: servers(true)}}
	up := svc.snapshotExtensions(context.Background())
	svc.MCPMgr = &stubMCPMgr{servers: servers(false)}
	down := svc.snapshotExtensions(context.Background())

	if up.extensionNames() != down.extensionNames() {
		t.Fatalf("连接状态变化不得改变稳定层名称清单:\n%q\n%q", up.extensionNames(), down.extensionNames())
	}
	if want := "Plugins: Alpha, zeta | MCPs: amber, beta"; up.extensionNames() != want {
		t.Fatalf("名称清单应按名称排序: %q, want %q", up.extensionNames(), want)
	}
	for _, mark := range []string{"✓", "~", "✗"} {
		if strings.Contains(up.extensionNames(), mark) {
			t.Fatalf("稳定层不得含连接状态标记 %s: %q", mark, up.extensionNames())
		}
	}
	if up.extensionStatus() == down.extensionStatus() || !strings.Contains(up.extensionStatus(), "✓") {
		t.Fatalf("连接状态应进入易变层并随连接变化: %q vs %q", up.extensionStatus(), down.extensionStatus())
	}
}

func TestSortedValueLines_Deterministic(t *testing.T) {
	m := map[string]any{}
	for _, k := range []string{"k9", "k1", "k5", "k3", "k7", "k2", "k8", "k4", "k6", "k0"} {
		m[k] = "v-" + k
	}
	first := strings.Join(sortedValueLines(m), "|")
	for i := 0; i < 50; i++ {
		if got := strings.Join(sortedValueLines(m), "|"); got != first {
			t.Fatalf("用户画像渲染顺序不稳定: %q vs %q", first, got)
		}
	}
	if !strings.HasPrefix(first, "- v-k0|- v-k1") {
		t.Fatalf("应按键升序: %q", first)
	}
}
