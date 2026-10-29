package guard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
)

// 把 HOME 指到临时目录，使自保护/敏感路径落在可控位置。
func fakeHome(t *testing.T) (home string, layout config.DataLayout) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(config.EnvPolarisDataDir, "")
	l, err := config.DefaultDataLayout()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.VaultKeyFile, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, l
}

func TestCheckReadablePath_GrantWidensButSelfProtectedHolds(t *testing.T) {
	home, l := fakeHome(t)
	workspace := t.TempDir()
	outside := t.TempDir() // 可信根之外
	target := filepath.Join(outside, "go.mod")
	if err := os.WriteFile(target, []byte("module x"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{workspace}
	bg := context.Background()

	// 无凭据：工作区外读取被拒
	if err := CheckReadablePath(bg, target, base); err == nil {
		t.Fatal("无凭据读可信根外应拒绝")
	}
	// 有凭据（自动 / 显式）：放行
	for _, g := range []protocol.PermissionGrant{protocol.GrantAuto, protocol.GrantExplicit} {
		ctx := protocol.WithPermissionGrant(bg, g)
		if err := CheckReadablePath(ctx, target, base); err != nil {
			t.Fatalf("grant=%d 读工作区外应放行: %v", g, err)
		}
	}
	// 有凭据读 vault.key / secrets：仍拒绝
	ctx := protocol.WithPermissionGrant(bg, protocol.GrantExplicit)
	for _, p := range []string{l.VaultKeyFile, filepath.Join(l.Secrets, "x"), l.ConfigFile} {
		if err := CheckReadablePath(ctx, p, base); err == nil {
			t.Fatalf("自保护路径 %s 有凭据也必须拒绝", p)
		}
		if err := CheckWritablePath(ctx, p, base); err == nil {
			t.Fatalf("自保护路径 %s 写入有凭据也必须拒绝", p)
		}
	}
	// 敏感路径：无凭据拒绝（即便在白名单内），有凭据放行（由 Decide 硬限制询问）
	ssh := filepath.Join(home, ".ssh", "id_rsa")
	if err := CheckReadablePath(bg, ssh, []string{home}); err == nil {
		t.Fatal("无凭据读 ~/.ssh 应拒绝")
	}
	if err := CheckReadablePath(ctx, ssh, base); err != nil {
		t.Fatalf("有凭据读 ~/.ssh 由 Decide 负责询问，guard 不再拦: %v", err)
	}
}

func TestCheckReadablePath_SymlinkToSecretsRejected(t *testing.T) {
	_, l := fakeHome(t)
	workspace := t.TempDir()
	link := filepath.Join(workspace, "innocent")
	if err := os.Symlink(l.VaultKeyFile, link); err != nil {
		t.Fatal(err)
	}
	for _, g := range []protocol.PermissionGrant{protocol.GrantNone, protocol.GrantAuto, protocol.GrantExplicit} {
		ctx := protocol.WithPermissionGrant(context.Background(), g)
		if err := CheckReadablePath(ctx, link, []string{workspace}); err == nil {
			t.Fatalf("grant=%d: 指向 vault.key 的软链必须拒绝", g)
		}
	}
}

func TestCheckReadablePath_ConfinedRootNotWidenedByGrant(t *testing.T) {
	fakeHome(t)
	shared := t.TempDir()
	other := t.TempDir()
	ctx := protocol.WithPermissionGrant(protocol.WithConfinedRoot(context.Background(), shared), protocol.GrantExplicit)
	if err := CheckReadablePath(ctx, filepath.Join(other, "a"), ScopedPaths(ctx, nil)); err == nil {
		t.Fatal("受限根任务不得被授权凭据放宽")
	}
}

func TestSelfProtectedMatcher(t *testing.T) {
	_, l := fakeHome(t)
	m := SelfProtectedMatcher()
	if !m(filepath.Join(l.Secrets, "a", "b")) || !m(l.VaultKeyFile) {
		t.Fatal("secrets 与 vault.key 应命中")
	}
	if m(filepath.Join(l.Workspace, "a.txt")) {
		t.Fatal("workspace 不是自保护路径")
	}
}

// ADR-0122：无凭据时持久化落点与可信根外的私钥文件名同样拒绝；可信根内的 id_rsa 样例放行。
func TestCheckWritablePath_CatalogClasses(t *testing.T) {
	home, _ := fakeHome(t)
	workspace := t.TempDir()
	base := []string{workspace}
	bg := context.Background()

	if err := CheckWritablePath(bg, filepath.Join(home, ".zshrc"), []string{workspace, home}); err == nil {
		t.Error("无凭据写 ~/.zshrc 应拒绝")
	}
	hook := filepath.Join(workspace, ".git", "hooks", "pre-commit")
	if err := CheckWritablePath(bg, hook, base); err == nil {
		t.Error("无凭据写 .git/hooks 应拒绝")
	}
	if err := CheckReadablePath(bg, filepath.Join(workspace, ".git", "hooks", "pre-commit"), base); err != nil {
		t.Errorf("读 .git/hooks 应放行: %v", err)
	}
	if err := CheckReadablePath(bg, filepath.Join(workspace, "fixtures", "id_rsa"), base); err != nil {
		t.Errorf("可信根内 id_rsa 样例应放行: %v", err)
	}
}
