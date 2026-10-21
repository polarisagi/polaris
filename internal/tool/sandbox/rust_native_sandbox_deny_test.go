package sandbox

import (
	"reflect"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
)

func TestWithDefaultDenyPaths_InjectsWhenEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := withDefaultDenyPaths(protocol.SandboxContext{AllowedPaths: []string{"/w"}}).DenyPaths
	if len(got) == 0 {
		t.Fatal("DenyPaths 应被默认注入")
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Fatalf("存在重复路径 %q", p)
		}
		seen[p] = true
	}
	for _, p := range policy.SelfProtectedPaths() {
		if !seen[p] {
			t.Errorf("缺少自保护路径 %q", p)
		}
	}
	for _, p := range policy.SensitivePaths(false) {
		if !seen[p] {
			t.Errorf("缺少敏感路径 %q", p)
		}
	}
	// 系统目录读取必须保留，否则 bash 无法启动
	for _, p := range []string{"/usr", "/bin", "/etc"} {
		if seen[p] {
			t.Errorf("默认清单不得读写全拒系统目录 %q", p)
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
