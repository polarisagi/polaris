package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

// protectedCtx 把 HOME 指向临时目录，使自保护/敏感路径落在可控位置。
func protectedCtx(t *testing.T) (Context, config.DataLayout, string) {
	t.Helper()
	home := outsideDir(t)
	t.Setenv("HOME", home)
	t.Setenv(config.EnvPolarisDataDir, "")
	layout, err := config.DefaultDataLayout()
	if err != nil {
		t.Fatal(err)
	}
	root := outsideDir(t)
	return Context{Roots: []string{root}, Home: home}, layout, root
}

var toolGrep = types.Tool{Name: "grep", Capability: types.CapReadOnly}

func TestDecide_SelfProtected_DeniedInEveryMode(t *testing.T) {
	ctx, l, _ := protectedCtx(t)
	cases := []struct {
		name string
		tool types.Tool
		args []byte
	}{
		{"read vault.key", toolRead, jsonArgs(t, map[string]any{"path": l.VaultKeyFile})},
		{"read secrets", toolRead, jsonArgs(t, map[string]any{"path": filepath.Join(l.Secrets, "k")})},
		{"grep secrets dir", toolGrep, jsonArgs(t, map[string]any{"pattern": "x", "path": l.Secrets})},
		{"write config.toml", toolWrite, jsonArgs(t, map[string]any{"path": l.ConfigFile, "content": "x"})},
		{"bash cat vault.key", toolBash, jsonArgs(t, map[string]any{"command": "cat " + l.VaultKeyFile})},
		{"bash cat ~ form", toolBash, jsonArgs(t, map[string]any{"command": "cat ~/.polaris/vault.key"})},
		{"bash redirect", toolBash, jsonArgs(t, map[string]any{"command": "echo x > " + l.KillSwitchFile})},
		{"bash --opt=path", toolBash, jsonArgs(t, map[string]any{"command": "tar -cf out.tar --file=" + l.Data})},
	}
	for _, c := range cases {
		for _, mode := range []types.PermissionMode{types.ModeDefault, types.ModeAutoReview, types.ModeFullAccess} {
			d := Decide(mode, c.tool, c.args, types.TaintLow, ctx)
			if d.Verdict != VerdictDeny || d.Reason != ReasonSelfProtected {
				t.Errorf("%s/%s: got %+v, want Deny self_protected", c.name, mode, d)
			}
		}
	}
}

func TestDecide_SelfProtected_ViaSymlink(t *testing.T) {
	ctx, l, root := protectedCtx(t)
	if err := os.MkdirAll(l.Secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent")
	if err := os.Symlink(l.Secrets, link); err != nil {
		t.Fatal(err)
	}
	d := Decide(types.ModeFullAccess, toolRead, jsonArgs(t, map[string]any{"path": filepath.Join(link, "k")}), types.TaintLow, ctx)
	if d.Verdict != VerdictDeny {
		t.Fatalf("软链指向 secrets 必须 Deny, got %+v", d)
	}
}

func TestDecide_SensitivePath_HardLimitInEveryMode(t *testing.T) {
	ctx, _, _ := protectedCtx(t)
	ssh := filepath.Join(ctx.Home, ".ssh", "id_rsa")
	cases := []struct {
		name   string
		tool   types.Tool
		args   []byte
		reason string
	}{
		{"read ssh", toolRead, jsonArgs(t, map[string]any{"path": ssh}), ReasonSensitivePath},
		{"bash cat ssh", toolBash, jsonArgs(t, map[string]any{"command": "cat ~/.ssh/id_rsa"}), ReasonSensitivePath},
		{"write /etc", toolWrite, jsonArgs(t, map[string]any{"path": "/etc/hosts", "content": "x"}), ReasonSystemPath},
	}
	for _, c := range cases {
		for _, mode := range []types.PermissionMode{types.ModeDefault, types.ModeAutoReview, types.ModeFullAccess} {
			d := Decide(mode, c.tool, c.args, types.TaintLow, ctx)
			if d.Verdict != VerdictAsk || !d.HardLimit || d.Reason != c.reason {
				t.Errorf("%s/%s: got %+v, want hard-limit Ask %s", c.name, mode, d, c.reason)
			}
		}
	}
}

// 读 /etc 只是读，不触发写口径的系统目录硬限制。
func TestDecide_ReadSystemDirIsNotSensitive(t *testing.T) {
	ctx, _, _ := protectedCtx(t)
	d := Decide(types.ModeDefault, toolRead, jsonArgs(t, map[string]any{"path": "/etc/hosts"}), types.TaintLow, ctx)
	if d.Verdict != VerdictAllow {
		t.Fatalf("got %+v", d)
	}
}

// 现场复现：full_access + 工作区外的仓库目录读取，Decide 必须放行（三档都放行，ADR-0117 读取类自动）。
func TestDecide_ReadOutsideRoots_AllowedInAllModes(t *testing.T) {
	ctx, _, _ := protectedCtx(t)
	repoDir := outsideDir(t)
	for _, mode := range []types.PermissionMode{types.ModeDefault, types.ModeAutoReview, types.ModeFullAccess} {
		for _, tl := range []types.Tool{toolRead, toolGrep} {
			args := jsonArgs(t, map[string]any{"path": filepath.Join(repoDir, "go.mod"), "pattern": "x"})
			if d := Decide(mode, tl, args, types.TaintLow, ctx); d.Verdict != VerdictAllow || d.Auto != "read" {
				t.Errorf("%s/%s: got %+v", tl.Name, mode, d)
			}
		}
	}
	// 读取类缺 path 不得按根外处理
	if d := Decide(types.ModeDefault, toolRead, []byte(`{}`), types.TaintLow, ctx); d.Verdict != VerdictAllow {
		t.Fatalf("缺 path 的读取类应放行, got %+v", d)
	}
}

func TestClassifyPath_SelfAndSystem(t *testing.T) {
	_, l, _ := protectedCtx(t)
	if !ClassifyPath(filepath.Join(l.Secrets, "x"), false, nil).Self {
		t.Error("secrets 应为自保护")
	}
	if ClassifyPath(l.Secrets+"x", false, nil).Self {
		t.Error("前缀混淆（secretsx）不应命中")
	}
	if ClassifyPath("/etc/hosts", false, nil).System {
		t.Error("读口径不含系统目录")
	}
	if runtime.GOOS != "windows" && !ClassifyPath("/etc/hosts", true, nil).System {
		t.Error("写口径含 /etc")
	}
}

// browserLoginData 当前平台 Chrome 的 Login Data 路径（windows 不在此测）。
func browserLoginData(t *testing.T, home string) string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Default", "Login Data")
	case "linux":
		return filepath.Join(home, ".config", "google-chrome", "Default", "Login Data")
	}
	t.Skip("仅 darwin/linux")
	return ""
}

func TestDecide_ADR0122_PathCatalog(t *testing.T) {
	ctx, _, root := protectedCtx(t)
	hardAsk := func(d Decision, reason string) bool {
		return d.Verdict == VerdictAsk && d.HardLimit && d.Reason == reason
	}
	// full_access 下读浏览器 Login Data 仍硬限制
	login := browserLoginData(t, ctx.Home)
	d := Decide(types.ModeFullAccess, toolRead, jsonArgs(t, map[string]any{"path": login}), types.TaintLow, ctx)
	if !hardAsk(d, ReasonSensitivePath) {
		t.Errorf("读 Login Data: got %+v", d)
	}
	// auto_review 下项目内写 .git/hooks/pre-commit 是持久化硬限制
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	d = Decide(types.ModeAutoReview, toolWrite, jsonArgs(t, map[string]any{"path": hook, "content": "x"}), types.TaintLow, ctx)
	if !hardAsk(d, ReasonPersistencePath) {
		t.Errorf("写 .git/hooks: got %+v", d)
	}
	// 写 ~/.zshrc 硬限制，读放行
	rc := filepath.Join(ctx.Home, ".zshrc")
	d = Decide(types.ModeFullAccess, toolWrite, jsonArgs(t, map[string]any{"path": rc, "content": "x"}), types.TaintLow, ctx)
	if !hardAsk(d, ReasonPersistencePath) {
		t.Errorf("写 .zshrc: got %+v", d)
	}
	d = Decide(types.ModeFullAccess, toolRead, jsonArgs(t, map[string]any{"path": rc}), types.TaintLow, ctx)
	if d.Verdict != VerdictAllow {
		t.Errorf("读 .zshrc 应放行: got %+v", d)
	}
	// 写系统目录：原因码为 system_path
	if runtime.GOOS != "windows" {
		d = Decide(types.ModeFullAccess, toolWrite, jsonArgs(t, map[string]any{"path": "/etc/hosts", "content": "x"}), types.TaintLow, ctx)
		if !hardAsk(d, ReasonSystemPath) {
			t.Errorf("写 /etc/hosts: got %+v", d)
		}
	}
	// shell 重定向写 .git/hooks 同样硬限制；读 ~/.zshrc 放行
	cmd := "echo x > " + hook
	d = Decide(types.ModeFullAccess, toolBash, jsonArgs(t, map[string]any{"command": cmd}), types.TaintLow, ctx)
	if !hardAsk(d, ReasonPersistencePath) {
		t.Errorf("bash 重定向写 hook: got %+v", d)
	}
	d = Decide(types.ModeFullAccess, toolBash, jsonArgs(t, map[string]any{"command": "cat ~/.zshrc"}), types.TaintLow, ctx)
	if d.Verdict != VerdictAllow {
		t.Errorf("bash cat ~/.zshrc 应放行: got %+v", d)
	}
}
