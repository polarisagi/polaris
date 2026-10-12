package server

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestMeshSettings_SaveHotAppliesAndPersists 设置页保存：写回 config.toml（保留注释）、节点名进 Agent Card、
// 新监听地址立即可访问、对端策略立即生效；只有 enabled 变化要求重启。
func TestMeshSettings_SaveHotAppliesAndPersists(t *testing.T) {
	n := newPairNode(t, "laptop")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("# mine\n[a2a]\nenabled = true\nname = \"laptop\"  # 显示名\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.s.SetConfigPath(cfgPath)
	n.s.dataDir = t.TempDir()
	n.s.srv = &http.Server{Handler: n.srv.Config.Handler} //nolint:gosec // 测试服务器
	t.Cleanup(func() { _ = n.s.srv.Close() })

	code, got := n.call(t, http.MethodGet, "/v1/mesh/settings", nil)
	if code != http.StatusOK || got["running"] != true || got["settings"].(map[string]any)["node_name"] != "laptop" {
		t.Fatalf("get settings: %d %v", code, got)
	}

	listen := freeLocalAddr(t)
	shared := t.TempDir()
	code, got = n.call(t, http.MethodPut, "/v1/mesh/settings", map[string]any{
		"enabled": true, "node_name": "书房的 Mac", "extra_listen": []string{listen},
		"peer_read_only": true, "allow_relay": true, "peer_file_root": shared,
	})
	if code != http.StatusOK || got["restart_required"] != false {
		t.Fatalf("put settings: %d %v", code, got)
	}
	raw, _ := os.ReadFile(cfgPath)
	for _, want := range []string{"# mine", "name = '书房的 Mac'  # 显示名", "extra_listen = ['" + listen + "']", "allow_relay = true"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config.toml missing %q:\n%s", want, raw)
		}
	}
	if pol := n.svc.Policy(); !pol.AllowRelay || pol.FileRoot != shared {
		t.Fatalf("peer policy not hot-applied: %+v", pol)
	}
	resp, err := http.Get("http://" + listen + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("new listener must serve immediately: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), "书房的 Mac") {
		t.Fatalf("agent card must carry the new node name: %v %s", err, body)
	}

	// 去掉监听地址：监听器关闭。
	if code, got = n.call(t, http.MethodPut, "/v1/mesh/settings", map[string]any{
		"enabled": true, "node_name": "书房的 Mac", "extra_listen": []string{}, "peer_read_only": true,
	}); code != http.StatusOK {
		t.Fatalf("remove listen: %d %v", code, got)
	}
	if c, err := net.Dial("tcp", listen); err == nil {
		_ = c.Close()
		t.Fatal("removed listener must be closed")
	}

	// 关闭 a2a：只写配置，提示重启。
	if code, got = n.call(t, http.MethodPut, "/v1/mesh/settings", map[string]any{
		"enabled": false, "node_name": "书房的 Mac", "extra_listen": []string{}, "peer_read_only": true,
	}); code != http.StatusOK || got["restart_required"] != true {
		t.Fatalf("toggling enabled must require restart: %d %v", code, got)
	}
	if cfg, err := config.Load(cfgPath); err != nil || cfg.A2A.Enabled {
		t.Fatalf("enabled=false must be persisted: %v %v", cfg, err)
	}
}

func TestMeshSettings_RejectsBadInput(t *testing.T) {
	n := newPairNode(t, "laptop")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	n.s.SetConfigPath(cfgPath)
	home, _ := os.UserHomeDir()
	for name, body := range map[string]map[string]any{
		"empty name":      {"enabled": true, "node_name": " "},
		"wildcard listen": {"enabled": true, "node_name": "a", "extra_listen": []string{"0.0.0.0:28888"}},
		"hostname listen": {"enabled": true, "node_name": "a", "extra_listen": []string{"localhost:28888"}},
		"foreign ip":      {"enabled": true, "node_name": "a", "extra_listen": []string{"203.0.113.9:28888"}},
		"relative root":   {"enabled": true, "node_name": "a", "peer_file_root": "shared"},
		"home as root":    {"enabled": true, "node_name": "a", "peer_file_root": home},
	} {
		if code, got := n.call(t, http.MethodPut, "/v1/mesh/settings", body); code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %v", name, code, got)
		}
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("rejected settings must not create or modify config.toml")
	}
}
