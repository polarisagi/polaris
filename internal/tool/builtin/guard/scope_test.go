package guard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
)

func TestCheckScopeRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 HOME")
	}
	ok := []string{filepath.Join(home, "code", "polaris"), t.TempDir()}
	for _, p := range ok {
		if err := CheckScopeRoot(p); err != nil {
			t.Errorf("%s 应允许: %v", p, err)
		}
	}
	bad := []string{
		home,                                // 主目录本身：~/.ssh 在其下
		filepath.Dir(home),                  // 主目录的上级
		"/",                                 // 根
		filepath.Join(home, ".ssh"),         // 敏感目录本身
		filepath.Join(home, ".ssh", "keys"), // 敏感目录内
		filepath.Join(home, config.DefaultDataDirRel), // 敏感目录的祖先
		"/etc",         // 系统目录
		"relative/dir", // 非绝对路径
	}
	// macOS：/etc 是 /private/etc 的软链，规范化后的项目根是后者，同样须拒绝。
	if real, err := filepath.EvalSymlinks("/etc"); err == nil && real != "/etc" {
		bad = append(bad, real)
	}
	for _, p := range bad {
		if err := CheckScopeRoot(p); err == nil {
			t.Errorf("%s 应拒绝", p)
		}
	}
}

func TestScopedPathsAndSearchRoots(t *testing.T) {
	base := []string{"/data/polaris"}
	root := t.TempDir()

	if got := ScopedPaths(context.Background(), base); len(got) != 1 || got[0] != base[0] {
		t.Fatalf("无项目时应原样返回白名单，got %v", got)
	}
	ctx := protocol.WithProjectRoot(context.Background(), root)
	got := ScopedPaths(ctx, base)
	if len(got) != 2 || got[0] != root || got[1] != base[0] {
		t.Fatalf("项目根应置首位并保留白名单，got %v", got)
	}
	if sr := SearchRoots(ctx, base); len(sr) != 1 || sr[0] != root {
		t.Fatalf("项目会话默认只搜项目目录，got %v", sr)
	}
	// 纵深防御：ctx 里的根若是敏感目录的祖先（写入侧校验被绕过/规则收紧），不授予访问。
	if home, err := os.UserHomeDir(); err == nil {
		evil := protocol.WithProjectRoot(context.Background(), home)
		if got := ScopedPaths(evil, base); len(got) != 1 {
			t.Fatalf("主目录不得成为访问根，got %v", got)
		}
	}
}

// TestScopedPaths_ReadGateEndToEnd 读门控随项目根放宽，且仅对该会话 ctx 生效。
func TestScopedPaths_ReadGateEndToEnd(t *testing.T) {
	base := []string{t.TempDir()}
	root := t.TempDir()
	target := filepath.Join(root, "main.go")

	if err := CheckAllowedPath(target, ScopedPaths(context.Background(), base)); err == nil {
		t.Fatal("非项目会话不得访问项目目录")
	}
	ctx := protocol.WithProjectRoot(context.Background(), root)
	if err := CheckAllowedPath(target, ScopedPaths(ctx, base)); err != nil {
		t.Fatalf("项目会话应可访问项目目录: %v", err)
	}
	if err := CheckWritablePath(ctx, target, ScopedPaths(ctx, base)); err != nil {
		t.Fatalf("项目会话应可写项目目录: %v", err)
	}
}

// TestConfinedRoot_ReplacesAllowlist 受限根（对端委派任务）替换而不是追加白名单：DataDir 里的
// vault.key / polaris.db 不可达，项目目录也不可达；根本身非法时一律拒绝，不回落到白名单。
func TestConfinedRoot_ReplacesAllowlist(t *testing.T) {
	dataDir := t.TempDir()
	shared := filepath.Join(dataDir, "workspace", "peer-shared")
	project := t.TempDir()
	base := []string{dataDir}

	ctx := protocol.WithProjectRoot(protocol.WithConfinedRoot(context.Background(), shared), project)
	if err := CheckAllowedPath(filepath.Join(shared, "notes.md"), ScopedPaths(ctx, base)); err != nil {
		t.Fatalf("shared dir must stay readable: %v", err)
	}
	for _, p := range []string{filepath.Join(dataDir, "vault.key"), filepath.Join(dataDir, "data", "polaris.db"),
		filepath.Join(project, "main.go")} {
		if err := CheckAllowedPath(p, ScopedPaths(ctx, base)); err == nil {
			t.Fatalf("%s must not be reachable from a confined task", p)
		}
	}
	if sr := SearchRoots(ctx, base); len(sr) != 1 || sr[0] != shared {
		t.Fatalf("search must default to the confined root only, got %v", sr)
	}
	bad := protocol.WithConfinedRoot(context.Background(), "relative/dir")
	if got := ScopedPaths(bad, base); len(got) != 0 {
		t.Fatalf("an invalid confined root must yield no roots (fail-closed), got %v", got)
	}
	if err := CheckAllowedPath(filepath.Join(dataDir, "vault.key"), ScopedPaths(bad, base)); err == nil {
		t.Fatal("invalid confined root must not fall back to the process allowlist")
	}
}
