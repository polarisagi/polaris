package policy

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/config"
)

// 路径分类单一事实源（ADR-0121 决策四）：自保护路径任何模式拒绝，敏感路径任何模式硬限制。
// Decide、文件工具 guard、沙箱 DenyPaths 都从这里取清单，禁止各自维护。

// withReal 同时收录各项的真实路径：macOS 上 /etc → /private/etc，调用方传入的往往已
// EvalSymlinks，只比字面值会漏。
func withReal(paths []string) []string {
	out := make([]string, 0, len(paths)*2)
	seen := make(map[string]struct{}, len(paths)*2)
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range paths {
		p = filepath.Clean(p)
		add(p)
		add(resolveReal(p))
	}
	return out
}

func selfLayouts() []config.DataLayout {
	var out []config.DataLayout
	if l, err := config.DefaultDataLayout(); err == nil {
		out = append(out, l)
	}
	// 环境变量重定向数据根时，默认布局之外的真实数据根同样受保护
	if d := os.Getenv(config.EnvPolarisDataDir); d != "" {
		out = append(out, config.NewDataLayout(config.ExpandHome(d), config.DirsConfig{}))
	}
	return out
}

// SelfProtectedPaths Polaris 自身的密钥、配置、开关与数据目录。
func SelfProtectedPaths() []string {
	layouts := selfLayouts()
	base := make([]string, 0, len(layouts)*11)
	for _, l := range layouts {
		base = append(base,
			l.VaultKeyFile, l.ConfigFile, l.KillSwitchFile, l.FullStopFile, l.CLISessionFile,
			l.Secrets, l.Data, l.Audit, l.Eval, l.Config,
			filepath.Join(l.HarnessOverlay, "prompt_addenda"))
	}
	return withReal(base)
}

func sensitivePathsFor(home string, write bool) []string {
	var base []string
	if home != "" {
		base = append(base,
			filepath.Join(home, ".ssh"), filepath.Join(home, ".gnupg"), filepath.Join(home, ".aws"),
			filepath.Join(home, ".netrc"), filepath.Join(home, "Library", "Keychains"))
	}
	if write {
		base = append(base, "/etc", "/usr", "/bin", "/sbin")
	}
	return withReal(base)
}

// SensitivePaths 凭据类目录；write=true 时追加系统目录（只读系统目录无害，如 cat /etc/hosts）。
func SensitivePaths(write bool) []string {
	home, _ := os.UserHomeDir()
	return sensitivePathsFor(home, write)
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// PathClass 判定绝对路径 p 属于哪一类（前缀匹配：精确或紧跟分隔符）。
func PathClass(p string, write bool) (self, sensitive bool) {
	home, _ := os.UserHomeDir()
	return pathClassFor(p, write, home)
}

func pathClassFor(p string, write bool, home string) (self, sensitive bool) {
	p = filepath.Clean(p)
	selfList, sensList := SelfProtectedPaths(), sensitivePathsFor(home, write)
	// 软链指向受保护目录时，原路径与真实路径任一命中即算
	for _, c := range []string{p, resolveReal(p)} {
		self = self || underAny(c, selfList)
		sensitive = sensitive || underAny(c, sensList)
	}
	return self, sensitive
}
