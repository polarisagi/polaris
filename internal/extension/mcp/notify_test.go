package mcp

import (
	"encoding/json"
	"testing"
)

func TestServerMetaDeclaresExperimental(t *testing.T) {
	m := ServerMeta{Experimental: map[string]json.RawMessage{"claude/channel": json.RawMessage(`{}`),
		"claude/channel/permission": json.RawMessage(`false`)}}
	if !m.DeclaresExperimental("claude/channel") || m.DeclaresExperimental("claude/channel/permission") || m.DeclaresExperimental("x") {
		t.Fatal("only object values declare a capability; false opts out")
	}
}

func TestDispatchRoutesServerNotifications(t *testing.T) {
	c := &MCPClient{pending: map[int64]chan *mcpRPCResponse{}}
	var got string
	c.SetNotificationHandler(func(method string, params json.RawMessage) { got = method + string(params) })
	c.dispatch(&mcpRPCResponse{Method: "notifications/claude/channel", Params: json.RawMessage(`{"content":"x"}`)})
	if got != `notifications/claude/channel{"content":"x"}` {
		t.Fatalf("notification not routed: %q", got)
	}
}
