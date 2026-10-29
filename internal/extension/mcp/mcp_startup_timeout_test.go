package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// slowStdioScript 每条请求延迟 0.6s 后回空结果，模拟首次 uv/npx 装依赖期间的慢响应。
const slowStdioScript = `while IFS= read -r l; do
  id=$(printf '%s' "$l" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -z "$id" ] && continue
  sleep 0.6
  printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
done`

// 握手使用 StartupTimeout，握手后的普通请求仍受 Timeout 约束。
func TestInitialize_UsesStartupTimeoutNotRequestTimeout(t *testing.T) {
	c := NewMCPClient(MCPClientConfig{
		ServerName: "slow", Transport: MCPStdio, Command: "/bin/sh", Args: []string{"-c", slowStdioScript},
		TrustTier: 4, Timeout: 300 * time.Millisecond, StartupTimeout: 5 * time.Second,
	}, testSafeHTTP(nil))
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize must wait up to StartupTimeout: %v", err)
	}
	_, err := c.call(context.Background(), "ping", nil)
	if err == nil || !strings.Contains(err.Error(), "request timeout") {
		t.Fatalf("post-handshake call must use request Timeout, got %v", err)
	}
}

func TestNewMCPClient_StartupTimeoutDefault(t *testing.T) {
	c := NewMCPClient(MCPClientConfig{ServerName: "d", Transport: MCPStreamableHTTP, URL: "https://x/mcp"}, testSafeHTTP(nil))
	if c.cfg.StartupTimeout != DefaultStartupTimeout {
		t.Fatalf("default startup timeout: %v", c.cfg.StartupTimeout)
	}
}
