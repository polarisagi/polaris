package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
)

func TestWithDefaultDenyPaths_InjectsWhenEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := withDefaultDenyPaths(protocol.SandboxContext{AllowedPaths: []string{"/w"}})
	if len(got.DenyPaths) == 0 || len(got.DenyWritePaths) == 0 {
		t.Fatal("DenyPaths / DenyWritePaths 应被默认注入")
	}
	rw, wo := policy.SandboxDenyPaths()
	index := func(list []string) map[string]bool {
		seen := map[string]bool{}
		for _, p := range list {
			if seen[p] {
				t.Fatalf("存在重复路径 %q", p)
			}
			seen[p] = true
		}
		return seen
	}
	seenRW, seenWO := index(got.DenyPaths), index(got.DenyWritePaths)
	for _, p := range rw {
		if !seenRW[p] {
			t.Errorf("DenyPaths 缺少 %q", p)
		}
	}
	for _, p := range wo {
		if !seenWO[p] {
			t.Errorf("DenyWritePaths 缺少 %q", p)
		}
	}
	// 系统目录读取必须保留，否则 bash 无法启动：只能进只拒写清单
	for _, p := range []string{"/usr", "/bin", "/etc"} {
		if seenRW[p] {
			t.Errorf("默认清单不得读写全拒系统目录 %q", p)
		}
	}
}

func TestWithDefaultDenyPaths_ProjectGitPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	hooks := filepath.Join(root, ".git", "hooks")
	cfg := filepath.Join(root, ".git", "config")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[core]"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, in := range []protocol.SandboxContext{
		{AllowedPaths: []string{root}},
		{Workdir: root, DenyWritePaths: []string{"/x"}}, // 显式清单也要追加
	} {
		got := withDefaultDenyPaths(in).DenyWritePaths
		seen := map[string]bool{}
		for _, p := range got {
			seen[p] = true
		}
		if !seen[hooks] || !seen[cfg] {
			t.Errorf("应追加项目 .git/hooks 与 .git/config, got %v", got)
		}
	}
	// 不存在的 .git 不追加
	empty := t.TempDir()
	for _, p := range withDefaultDenyPaths(protocol.SandboxContext{AllowedPaths: []string{empty}}).DenyWritePaths {
		if filepath.Dir(filepath.Dir(p)) == empty {
			t.Errorf("不存在的路径不应追加: %q", p)
		}
	}
}

func TestWithDefaultDenyPaths_KeepsExplicit(t *testing.T) {
	in := protocol.SandboxContext{DenyPaths: []string{"/x"}}
	if got := withDefaultDenyPaths(in).DenyPaths; !reflect.DeepEqual(got, []string{"/x"}) {
		t.Fatalf("显式 DenyPaths 不应被覆盖: %v", got)
	}
}

func TestMergeDenyPaths_Dedup(t *testing.T) {
	got := mergeDenyPaths([]string{"/a", "", "/b"}, []string{"/b", "/c"})
	if !reflect.DeepEqual(got, []string{"/a", "/b", "/c"}) {
		t.Fatalf("got %v", got)
	}
}
