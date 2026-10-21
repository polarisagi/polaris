package policy

import (
	"os"
	"path/filepath"
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
		name string
		tool types.Tool
		args []byte
	}{
		{"read ssh", toolRead, jsonArgs(t, map[string]any{"path": ssh})},
		{"bash cat ssh", toolBash, jsonArgs(t, map[string]any{"command": "cat ~/.ssh/id_rsa"})},
		{"write /etc", toolWrite, jsonArgs(t, map[string]any{"path": "/etc/hosts", "content": "x"})},
	}
	for _, c := range cases {
		for _, mode := range []types.PermissionMode{types.ModeDefault, types.ModeAutoReview, types.ModeFullAccess} {
			d := Decide(mode, c.tool, c.args, types.TaintLow, ctx)
			if d.Verdict != VerdictAsk || !d.HardLimit || d.Reason != ReasonSensitivePath {
				t.Errorf("%s/%s: got %+v, want hard-limit Ask sensitive_path", c.name, mode, d)
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

func TestPathClass(t *testing.T) {
	_, l, _ := protectedCtx(t)
	if self, _ := PathClass(filepath.Join(l.Secrets, "x"), false); !self {
		t.Error("secrets 应为自保护")
	}
	if self, _ := PathClass(l.Secrets+"x", false); self {
		t.Error("前缀混淆（secretsx）不应命中")
	}
	if _, sens := PathClass("/etc/hosts", false); sens {
		t.Error("读口径不含 /etc")
	}
	if _, sens := PathClass("/etc/hosts", true); !sens {
		t.Error("写口径含 /etc")
	}
}
