package sandboxenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func BaseEnv() []string {
	// 构建安全 PATH（继承宿主 + 追加常见工具目录）
	sandboxPath := BuildSandboxEnvPath()

	// 基础安全变量
	vars := []string{
		"PATH=" + sandboxPath,
		"TMPDIR=/tmp",
		"TEMP=/tmp",
	}

	// HOME：使用真实 home（工具链需要 ~/.cargo 等目录）
	if home, err := os.UserHomeDir(); err == nil {
		vars = append(vars, "HOME="+home)
	} else {
		vars = append(vars, "HOME=/tmp")
	}

	// 安全白名单变量透传
	safePassthrough := []string{
		"LANG", "LC_ALL", "LC_CTYPE",
		"TZ", "USER", "LOGNAME",
		"PYTHONPATH", "PYTHONDONTWRITEBYTECODE", "VIRTUAL_ENV",
		"NODE_PATH", "NODE_ENV",
		"GOPATH", "GOROOT", "GOMODCACHE", "GOCACHE",
		"CARGO_HOME", "RUSTUP_HOME",
		"JAVA_HOME",
	}
	for _, key := range safePassthrough {
		if val := os.Getenv(key); val != "" {
			vars = append(vars, key+"="+val)
		}
	}

	// 高危变量黑名单（显式排除，确保不通过 os.Environ 漏入）
	// 此函数不调用 os.Environ()，仅白名单透传，因此黑名单是 defense-in-depth。
	return vars
}

func BuildSandboxEnvPath() string {
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			parts = append(parts, p)
			seen[p] = true
		}
	}

	// 1. 继承宿主 PATH（最重要：pyenv/nvm/cargo shims 等需要宿主 PATH）
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		add(p)
	}
	// 2. 平台基础目录（保底）
	for _, p := range []string{"/usr/local/bin", "/usr/local/sbin", "/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		add(p)
	}
	// 3. macOS Homebrew
	if runtime.GOOS == "darwin" {
		add("/opt/homebrew/bin")
		add("/opt/homebrew/sbin")
	}
	// 4–6. nix + 用户工具目录 + Linux 特定（各自提取以降低圈复杂度）
	SandboxPathAddNix(add)
	SandboxPathAddUserTools(add)
	SandboxPathAddLinux(add)

	return strings.Join(parts, string(filepath.ListSeparator))
}

func SandboxPathAddNix(add func(string)) {
	for _, p := range []string{
		"/nix/var/nix/profiles/default/bin",
		"/run/current-system/sw/bin",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
}

func SandboxPathAddUserTools(add func(string)) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, p := range []string{
		home + "/.cargo/bin",
		home + "/.local/bin",
		home + "/go/bin",
		home + "/.go/bin",
		home + "/.pyenv/shims",
		home + "/.pyenv/bin",
		home + "/.deno/bin",
		home + "/.bun/bin",
		home + "/.asdf/shims",
		home + "/.asdf/bin",
		home + "/.rye/shims",
		home + "/.local/share/mise/shims",
		home + "/.rbenv/shims",
		home + "/.rbenv/bin",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
	// NVM：动态解析活跃版本（~/.nvm/alias/default 记录当前版本）。
	// 旧的 ~/.nvm/versions/node/current/bin 是 symlink，NVM 不保证其存在。
	if nvmBin := NvmNodeBinPath(home); nvmBin != "" {
		add(nvmBin)
	}
}

func NvmNodeBinPath(home string) string {
	nvmVersionsDir := filepath.Join(home, ".nvm", "versions", "node")
	// 1. 读别名文件解析活跃版本
	if p := NvmActiveVersionBinPath(home, nvmVersionsDir); p != "" {
		return p
	}
	// 2. 降级：扫描目录，取字典序最大的 v* 目录（近似最新版本）
	entries, err := os.ReadDir(nvmVersionsDir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
			p := filepath.Join(nvmVersionsDir, e.Name(), "bin")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func NvmActiveVersionBinPath(home, nvmVersionsDir string) string {
	aliasPath := filepath.Join(home, ".nvm", "alias", "default")
	data, err := os.ReadFile(aliasPath)
	if err != nil {
		return ""
	}
	version := strings.TrimSpace(string(data))
	// alias 可能指向 lts/* 二级别名，追踪一层
	if strings.HasPrefix(version, "lts/") || (!strings.HasPrefix(version, "v") && !strings.Contains(version, ".")) {
		ltsAlias := filepath.Join(home, ".nvm", "alias", version)
		if ltsData, err2 := os.ReadFile(ltsAlias); err2 == nil {
			version = strings.TrimSpace(string(ltsData))
		}
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	p := filepath.Join(nvmVersionsDir, version, "bin")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func SandboxPathAddLinux(add func(string)) {
	if runtime.GOOS != "linux" {
		return
	}
	for _, p := range []string{
		"/snap/bin",
		"/opt/conda/bin", "/opt/miniconda3/bin", "/opt/anaconda3/bin",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
}
