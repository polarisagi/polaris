package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const patchSample = `# 用户配置
[a2a]
enabled = true
name = "polaris-agent-0"   # 显示名

[[a2a.skill]]
id = "x"
name = "y"

[interface]
host = "127.0.0.1"
port = 28888
extra_listen = []

[mesh]
peer_read_only = true          # 对端任务只读
allow_relay = false
`

func writeSample(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(patchSample), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPatchUserConfig_EditsInPlaceAndKeepsComments(t *testing.T) {
	p := writeSample(t)
	err := PatchUserConfig(p, []KeyPatch{
		{"a2a", "name", `desk "top"`},
		{"interface", "extra_listen", []string{"100.101.102.103:28888"}},
		{"mesh", "allow_relay", true},
		{"mesh", "peer_file_root", "/srv/shared"}, // 表内缺失的键：追加
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	got := string(raw)
	for _, want := range []string{
		"# 用户配置",
		`name = 'desk "top"'  # 显示名`,
		"[[a2a.skill]]\nid = \"x\"", // 数组表原样
		`extra_listen = ['100.101.102.103:28888']`,
		"allow_relay = true",
		"peer_read_only = true          # 对端任务只读",
		`peer_file_root = '/srv/shared'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("patched file missing %q:\n%s", want, got)
		}
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.A2A.Name != `desk "top"` || len(cfg.Interface.ExtraListen) != 1 || !cfg.Mesh.AllowRelay || cfg.Mesh.PeerFileRoot != "/srv/shared" {
		t.Fatalf("values not applied: %+v %+v %+v", cfg.A2A.Name, cfg.Interface.ExtraListen, cfg.Mesh)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config must stay 0600, got %v", fi.Mode().Perm())
	}
}

func TestPatchUserConfig_InvalidResultLeavesFileUntouched(t *testing.T) {
	p := writeSample(t)
	// 通配监听未显式允许：Validate 拒绝，原文件不得被改。
	if err := PatchUserConfig(p, []KeyPatch{{"interface", "extra_listen", []string{"0.0.0.0:28888"}}}); err == nil {
		t.Fatal("invalid config must be rejected")
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != patchSample {
		t.Fatal("rejected patch must not modify the file")
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp files must be cleaned up, got %d entries", len(entries))
	}
}

func TestPatchUserConfig_MissingSectionAndFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[interface]\nport = 28888\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PatchUserConfig(p, []KeyPatch{{"mesh", "allow_relay", true}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.HasSuffix(string(raw), "[mesh]\nallow_relay = true\n") {
		t.Fatalf("missing section must be appended, got:\n%s", raw)
	}
	// 文件不存在：以内嵌 defaults 为底稿。
	p2 := filepath.Join(t.TempDir(), "sub", "config.toml")
	if err := PatchUserConfig(p2, []KeyPatch{{"a2a", "name", "vps"}}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(p2); err != nil || cfg.A2A.Name != "vps" {
		t.Fatalf("new file from defaults: %v %v", cfg, err)
	}
	if err := PatchUserConfig(p2, []KeyPatch{{"a2a]\nevil", "x", "y"}}); err == nil {
		t.Fatal("section names must be validated")
	}
}
