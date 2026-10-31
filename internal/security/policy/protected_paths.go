package policy

import (
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/config"
)

// 自保护路径生成（ADR-0121 决策四）：vault.key、config.toml、secrets/、data/ 等由 DataLayout
// 派生，不进目录文件（路径随数据根重定向而变）。分类与沙箱清单统一见 pathcatalog.go。

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
	// 配置可重定向数据根及受保护子目录；运行时实际布局必须与默认路径并集保护。
	if cfg := config.Get(); cfg != nil {
		out = append(out, cfg.Layout())
	}
	// 环境变量重定向数据根时，默认布局之外的真实数据根同样受保护
	if d := os.Getenv(config.EnvPolarisDataDir); d != "" {
		out = append(out, config.NewDataLayout(config.ExpandHome(d), config.DirsConfig{}))
	}
	return out
}

// selfPathsOf Polaris 自身的密钥、配置、开关与数据目录（读写都拒绝）。
func selfPathsOf(layouts []config.DataLayout) []string {
	base := make([]string, 0, len(layouts)*11)
	for _, l := range layouts {
		base = append(base,
			l.VaultKeyFile, l.ConfigFile, l.KillSwitchFile, l.FullStopFile, l.CLISessionFile,
			l.Secrets, l.Data, l.Audit, l.Eval, l.Config,
			filepath.Join(l.HarnessOverlay, "prompt_addenda"))
	}
	return withReal(base)
}

// selfExecPathsOf Polaris 自身的可执行扩展目录（hooks/extensions/skills）：可读，写入等同持久化。
func selfExecPathsOf(layouts []config.DataLayout) []string {
	base := make([]string, 0, len(layouts)*3)
	for _, l := range layouts {
		base = append(base, l.Hooks, l.Extensions, l.Skills)
	}
	return withReal(base)
}
