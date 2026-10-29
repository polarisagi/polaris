package lifecycle

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestResolvePluginTool(t *testing.T) {
	servers := []protocol.MCPServerInfo{
		{ID: "plugin_pl_aaaa1111_bridge", Name: "browser-bridge", Connected: true,
			Tools: []protocol.MCPTool{{Name: "bridge.setup"}, {Name: "bridge_status"}}},
		{ID: "plugin_pl_bbbb2222_other", Name: "other", Connected: true,
			Tools: []protocol.MCPTool{{Name: "bridge_status"}}},
		{ID: "mcp_user_srv", Name: "user-srv", Tools: []protocol.MCPTool{{Name: "bridge_status"}}},
	}

	t.Run("命名带非法字符的工具名按注册规则归一化", func(t *testing.T) {
		ref, ok := ResolvePluginTool(servers, "pl_aaaa1111", "bridge.setup")
		if !ok || ref.RegisteredName != "mcp__browser-bridge__bridge_setup" || ref.ServerID != "plugin_pl_aaaa1111_bridge" {
			t.Fatalf("got %+v ok=%v", ref, ok)
		}
	})
	t.Run("同名工具只在本插件服务器内解析", func(t *testing.T) {
		ref, ok := ResolvePluginTool(servers, "pl_bbbb2222", "bridge_status")
		if !ok || ref.RegisteredName != "mcp__other__bridge_status" {
			t.Fatalf("got %+v ok=%v", ref, ok)
		}
	})
	t.Run("未找到", func(t *testing.T) {
		if _, ok := ResolvePluginTool(servers, "pl_aaaa1111", "nope"); ok {
			t.Fatal("unexpected hit")
		}
		if _, ok := ResolvePluginTool(servers, "pl_cccc3333", "bridge_status"); ok {
			t.Fatal("tool of another plugin must not resolve")
		}
		if _, ok := ResolvePluginTool(servers, "", "bridge_status"); ok {
			t.Fatal("empty plugin id must not resolve")
		}
	})
}
