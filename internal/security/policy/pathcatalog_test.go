package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

// 三平台注入的家目录与环境变量（不触碰真实文件系统）。
func catalogFixture(goos string) (home string, getenv func(string) string) {
	switch goos {
	case "windows":
		env := map[string]string{
			"USERPROFILE":       `C:\Users\tester`,
			"APPDATA":           `C:\Users\tester\AppData\Roaming`,
			"LOCALAPPDATA":      `C:\Users\tester\AppData\Local`,
			"PROGRAMDATA":       `C:\ProgramData`,
			"SystemRoot":        `C:\Windows`,
			"ProgramFiles":      `C:\Program Files`,
			"ProgramFiles(x86)": `C:\Program Files (x86)`,
		}
		return env["USERPROFILE"], func(k string) string { return env[k] }
	case "darwin":
		return "/Users/tester", func(string) string { return "" }
	}
	return "/srv/tester", func(string) string { return "" }
}

func testCatalog(goos string) *PathCatalog {
	home, getenv := catalogFixture(goos)
	return newPathCatalog(goos, home, getenv, nil)
}

// sample 把目录文件里的一个路径模式变成一条具体的子路径样本。
func sample(c *PathCatalog, raw, home string, getenv func(string) string) (string, bool) {
	exp := c.expand(raw, home, "", getenv)
	if len(exp) == 0 {
		return "", false
	}
	p := strings.NewReplacer("*", "x", "?", "x").Replace(exp[0])
	sep := "/"
	if c.goos == "windows" {
		sep = `\`
	}
	return p + sep + "child", true
}

// 每个目录条目至少一个正例（含 anchors 与 basename 的文件名通配）。
func TestPathCatalog_EveryEntryHasPositive(t *testing.T) {
	files := loadCatalogFiles()
	for _, goos := range []string{"darwin", "linux", "windows"} {
		home, getenv := catalogFixture(goos)
		c := newPathCatalog(goos, home, getenv, nil)
		if c.invalid {
			t.Fatalf("%s: 目录文件解析失败", goos)
		}
		for _, name := range []string{"all", goos} {
			f := files[name]
			if f.err != nil {
				t.Fatalf("%s.yaml: %v", name, f.err)
			}
			for i, e := range f.entries {
				for _, raw := range e.Paths {
					p, ok := sample(c, raw, home, getenv)
					if !ok {
						t.Errorf("%s/%s#%d: %q 展开失败（环境变量缺失？）", goos, name, i, raw)
						continue
					}
					pc := c.Classify(p, true, nil)
					got := map[string]bool{"credential": pc.Credential, "persistence": pc.Persistence, "system": pc.System}[e.Class]
					if !got {
						t.Errorf("%s/%s#%d: %s 样本 %q 未命中, got %+v", goos, name, i, e.Class, p, pc)
					}
				}
				for _, a := range e.Anchors {
					p := "/proj/" + a + "/x"
					if !c.Classify(p, true, []string{"/proj"}).Persistence {
						t.Errorf("%s/%s#%d: anchor %q 未命中", goos, name, i, a)
					}
				}
				for _, pat := range e.Patterns {
					p := "/outside/" + strings.NewReplacer("*", "x").Replace(pat)
					if !c.Classify(p, false, []string{"/proj"}).Basename {
						t.Errorf("%s/%s#%d: basename %q 未命中", goos, name, i, pat)
					}
				}
				if e.Class == "" || e.Note == "" {
					t.Errorf("%s/%s#%d: 缺 class 或 note", goos, name, i)
				}
			}
		}
	}
}

func TestPathCatalog_CaseInsensitivity(t *testing.T) {
	d := testCatalog("darwin")
	if !d.Classify("/Users/tester/library/keychains/login.keychain-db", false, nil).Credential {
		t.Error("darwin 大小写变体应命中")
	}
	if !d.Classify("/users/TESTER/Library/Keychains", false, nil).Credential {
		t.Error("darwin 家目录大小写变体应命中")
	}
	l := testCatalog("linux")
	if l.Classify("/srv/tester/.SSH/id", false, nil).Credential {
		t.Error("linux 区分大小写，不应命中")
	}
	if !l.Classify("/srv/tester/.ssh/id", false, nil).Credential {
		t.Error("linux 精确路径应命中")
	}
	w := testCatalog("windows")
	if !w.Classify(`c:\users\tester\appdata\roaming\microsoft\credentials\x`, false, nil).Credential {
		t.Error("windows 小写盘符与目录应命中")
	}
	if !w.Classify(`C:/Windows/System32/config/SAM`, false, nil).Credential {
		t.Error("windows 正斜杠写法应命中")
	}
}

func TestPathCatalog_SegmentBoundaries(t *testing.T) {
	d := testCatalog("darwin")
	if !d.Classify("/Library/Foo", true, nil).System {
		t.Error("/Library/Foo 应为 system")
	}
	if d.Classify("/Users/tester/Library/Foo", true, nil).System {
		t.Error("~/Library/Foo 不得被 system /Library 命中")
	}
	if d.Classify("/Users/tester/.sshx/id", false, nil).Credential {
		t.Error("前缀混淆不应命中")
	}
}

func TestPathCatalog_ReadVsWrite(t *testing.T) {
	l := testCatalog("linux")
	rc := "/srv/tester/.zshrc"
	if pc := l.Classify(rc, false, nil); pc.Persistence {
		t.Errorf("读 .zshrc 不应命中 persistence: %+v", pc)
	}
	if !l.Classify(rc, true, nil).Persistence {
		t.Error("写 .zshrc 应命中 persistence")
	}
	if l.Classify("/etc/hosts", false, nil).System {
		t.Error("读 /etc 不应命中 system")
	}
}

func TestPathCatalog_Anchors(t *testing.T) {
	c := testCatalog("linux")
	roots := []string{"/work/proj"}
	for _, p := range []string{"/work/proj/.git/hooks/pre-commit", "/elsewhere/a/b/.git/config", "/work/proj/sub/.GIT/hooks"} {
		want := !strings.Contains(p, ".GIT") // linux 区分大小写
		if got := c.Classify(p, true, roots).Persistence; got != want {
			t.Errorf("%s: persistence=%v want %v", p, got, want)
		}
	}
	if c.Classify("/work/proj/.git/HEAD", true, roots).Persistence {
		t.Error(".git/HEAD 不是锚点")
	}
	if c.Classify("/work/proj/.git/hooks/pre-commit", false, roots).Persistence {
		t.Error("读 hook 不应命中")
	}
}

func TestPathCatalog_Basename(t *testing.T) {
	c := testCatalog("linux")
	roots := []string{"/work/proj"}
	if c.Classify("/work/proj/test/id_rsa", false, roots).Basename {
		t.Error("可信根内的 id_rsa 不应命中 basename")
	}
	if !c.Classify("/tmp/backup/id_ed25519.pub", false, roots).Basename {
		t.Error("根外 id_ed25519* 应命中")
	}
	if !c.Classify("/tmp/x/vault.KDBX", false, nil).Basename && c.fold {
		t.Error("折叠平台 .kdbx 大小写变体应命中")
	}
	d := testCatalog("darwin")
	if !d.Classify("/tmp/x/Vault.KDBX", false, nil).Basename {
		t.Error("darwin 大小写不敏感")
	}
}

// 软链指向 credential 目录，经真实路径命中；basename 软链同理。
func TestPathCatalog_SymlinkToCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("软链需要特权")
	}
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	link := filepath.Join(root, "innocent")
	if err := os.Symlink(ssh, link); err != nil {
		t.Fatal(err)
	}
	c := newPathCatalog(runtime.GOOS, home, os.Getenv, nil)
	if !c.Classify(filepath.Join(link, "key"), false, []string{root}).Credential {
		t.Error("软链指向 ~/.ssh 应命中 credential")
	}
	// 家目录本身是软链（macOS /var → /private/var）时，真实路径写法同样命中
	if real, err := filepath.EvalSymlinks(ssh); err == nil {
		if !c.Classify(filepath.Join(real, "key"), false, nil).Credential {
			t.Error("真实路径写法应命中")
		}
	}
}

func TestPathCatalog_SelfAndFragments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvPolarisDataDir, "")
	l, err := config.DefaultDataLayout()
	if err != nil {
		t.Fatal(err)
	}
	c := NewPathCatalog()
	if !c.Classify(l.VaultKeyFile, false, nil).Self {
		t.Error("vault.key 应为 Self")
	}
	if pc := c.Classify(filepath.Join(l.Hooks, "h.sh"), false, nil); pc.Persistence || pc.Self {
		t.Errorf("读 hooks 目录应放行, got %+v", pc)
	}
	if !c.Classify(filepath.Join(l.Hooks, "h.sh"), true, nil).Persistence {
		t.Error("写 hooks 目录应为 persistence")
	}
	frags := SensitiveFragments()
	has := func(f string) bool {
		for _, x := range frags {
			if x == f {
				return true
			}
		}
		return false
	}
	for _, f := range []string{".polaris/vault.key", ".polaris/secrets", "Login Data", "key4.db"} {
		if !has(f) {
			t.Errorf("缺少片段 %q", f)
		}
	}
}

func TestSandboxDenyPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvPolarisDataDir, "")
	l, err := config.DefaultDataLayout()
	if err != nil {
		t.Fatal(err)
	}
	rw, wo := SandboxDenyPaths()
	in := func(list []string, p string) bool {
		for _, x := range list {
			if x == p {
				return true
			}
		}
		return false
	}
	if !in(rw, l.VaultKeyFile) || !in(rw, filepath.Join(home, ".ssh")) {
		t.Errorf("rw 应含 self 与 credential: %v", rw)
	}
	if !in(wo, filepath.Join(home, ".zshrc")) || !in(wo, l.Hooks) {
		t.Errorf("writeOnly 应含 persistence 与自身 hooks: %v", wo)
	}
	for _, p := range append(append([]string{}, rw...), wo...) {
		if strings.Contains(p, ".git/hooks") {
			t.Errorf("anchors 不进沙箱清单: %q", p)
		}
	}
	for _, p := range []string{"/usr", "/bin", "/etc"} {
		if in(rw, p) {
			t.Errorf("系统目录不得读写全拒: %q", p)
		}
	}
	for _, p := range rw {
		if strings.ContainsAny(p, "*?[") {
			t.Errorf("通配应已展开: %q", p)
		}
	}
}

// 损坏的目录文件必须 fail-closed，而不是放行一切。
func TestPathCatalog_InvalidFailsClosed(t *testing.T) {
	c := &PathCatalog{goos: "linux", invalid: true}
	if !c.Classify("/tmp/anything", false, nil).Credential {
		t.Error("invalid 目录应 fail-closed")
	}
}
