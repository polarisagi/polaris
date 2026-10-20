package policy

import "strings"

func (t *cmdTable) addDevTools() {
	t.set(goHandler, "go")
	t.set(makeHandler, "make", "gmake")
	t.set(jsPkgHandler, "npm", "pnpm", "yarn", "bun")
	t.set(cargoHandler, "cargo")
	t.set(pythonHandler, "python", "python3")
	t.set(pipHandler, "pip", "pip3")
	t.set(nodeHandler, "node")
	t.set(workspaceToolHandler, "pytest", "ruff", "black", "mypy", "isort", "flake8", "pylint", "tsc", "eslint",
		"prettier", "jest", "vitest", "biome", "gofmt", "goimports", "golangci-lint", "staticcheck", "rustfmt", "shellcheck", "shfmt")
}

// argsStayInWorkspace 构建/测试类工具的字面路径参数不得指向可信根之外（动态参数放行：它们不是路径位置的可判定输入）。
func (c *cc) argsStayInWorkspace(args []string) bool {
	for _, a := range args {
		if hasDyn(a) {
			continue
		}
		p := a
		if i := strings.Index(a, "="); strings.HasPrefix(a, "-") && i > 0 {
			p = a[i+1:] // --out=/x
		}
		if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~") && !strings.Contains(p, "..") {
			continue
		}
		if c.w.pathState(p, false) != pathInside {
			return false
		}
	}
	return true
}

func (c *cc) subcommand() (string, []string) {
	for i, a := range c.args {
		if !strings.HasPrefix(a, "-") {
			return a, c.args[i+1:]
		}
	}
	return "", nil
}

func goHandler(c *cc) verdict {
	sub, rest := c.subcommand()
	ok := map[string]bool{"build": true, "test": true, "vet": true, "run": true, "fmt": true, "version": true,
		"list": true, "doc": true, "env": true, "mod": true}
	if !ok[sub] {
		return unknownV("go_subcommand")
	}
	if sub == "mod" {
		msub, _ := firstNonFlag(rest)
		if !map[string]bool{"tidy": true, "download": true, "verify": true, "graph": true, "why": true, "vendor": true}[msub] {
			return unknownV("go_mod_subcommand")
		}
	}
	if sub == "env" && (hasShort(rest, 'w') || hasShort(rest, 'u')) {
		return unknownV("go_env_write")
	}
	if !c.argsStayInWorkspace(c.args) {
		return unknownV("go_path_outside_workspace")
	}
	return safeV()
}

func firstNonFlag(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
	}
	return "", nil
}

func makeHandler(c *cc) verdict {
	for i := 0; i < len(c.args); i++ {
		a := c.args[i]
		if a == "-C" || a == "-f" || a == "--directory" || a == "--file" {
			if i+1 >= len(c.args) || c.pathsState([]string{c.args[i+1]}) != pathInside {
				return unknownV("make_path_outside_workspace")
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			continue
		}
		if isDeployTarget(a) {
			return unknownV("make_deploy_target")
		}
	}
	return safeV()
}

func isDeployTarget(t string) bool {
	for _, bad := range []string{"install", "uninstall", "deploy", "publish", "release", "push", "upload"} {
		if t == bad || strings.HasPrefix(t, bad+"-") || strings.HasPrefix(t, bad+"_") || strings.HasSuffix(t, "-"+bad) || strings.HasSuffix(t, "_"+bad) {
			return true
		}
	}
	return false
}

func jsPkgHandler(c *cc) verdict {
	if hasShort(c.args, 'g') || hasLong(c.args, "--global") {
		return unknownV("npm_global")
	}
	sub, _ := c.subcommand()
	if sub == "" {
		if c.prog == "yarn" {
			return safeV() // 裸 yarn = install
		}
		return unknownV("npm_usage")
	}
	ok := map[string]bool{"run": true, "run-script": true, "test": true, "t": true, "build": true, "ci": true,
		"install": true, "i": true}
	if ok[sub] {
		return safeV()
	}
	return unknownV("npm_subcommand")
}

func cargoHandler(c *cc) verdict {
	sub, _ := c.subcommand()
	if !map[string]bool{"build": true, "test": true, "check": true, "fmt": true, "clippy": true, "doc": true}[sub] {
		return unknownV("cargo_subcommand")
	}
	if !c.argsStayInWorkspace(c.args) {
		return unknownV("cargo_path_outside_workspace")
	}
	return safeV()
}

// scriptInWorkspace 解释器的第一个位置参数是否为工作区内的脚本文件。
func (c *cc) scriptInWorkspace(args []string, exts ...string) bool {
	pos := positionals(args)
	if len(pos) == 0 || hasDyn(pos[0]) {
		return false
	}
	ok := false
	for _, e := range exts {
		if strings.HasSuffix(pos[0], e) {
			ok = true
		}
	}
	return ok && c.w.pathState(pos[0], false) == pathInside
}

func pythonHandler(c *cc) verdict {
	for i, a := range c.args {
		if a == "-m" && i+1 < len(c.args) {
			return c.pythonModule(c.args[i+1], c.args[i+2:])
		}
		if a == "-c" {
			return unknownV("python_inline_code")
		}
	}
	if len(c.args) == 0 {
		return unknownV("python_repl")
	}
	if c.scriptInWorkspace(c.args, ".py") && c.argsStayInWorkspace(c.args) {
		return safeV()
	}
	return unknownV("python_script")
}

func (c *cc) pythonModule(mod string, rest []string) verdict {
	switch mod {
	case "pytest", "unittest", "compileall", "py_compile", "json.tool", "ruff", "black", "mypy":
		if c.argsStayInWorkspace(rest) {
			return safeV()
		}
		return unknownV("python_path_outside_workspace")
	case "pip":
		return c.pipInstall(rest)
	}
	return unknownV("python_module")
}

func pipHandler(c *cc) verdict { return c.pipInstall(c.args) }

// pipInstall pip 仅在工作区虚拟环境内的解释器/pip 下视为安全（避免写入系统或用户级 site-packages）。
func (c *cc) pipInstall(args []string) verdict {
	sub, _ := firstNonFlag(args)
	if !c.inVenv() {
		return unknownV("pip_outside_venv")
	}
	if sub == "install" || sub == "list" || sub == "show" || sub == "freeze" || sub == "check" {
		if hasLong(args, "--user", "--target", "--prefix", "--root") {
			return unknownV("pip_custom_location")
		}
		return safeV()
	}
	return unknownV("pip_subcommand")
}

func (c *cc) inVenv() bool {
	w0 := c.words[0]
	if !strings.Contains(w0, "/") {
		return false
	}
	return (strings.Contains(w0, "venv") || strings.Contains(w0, ".env")) && c.w.pathState(w0, hasDyn(w0)) == pathInside
}

func nodeHandler(c *cc) verdict {
	if hasShort(c.args, 'e') || hasShort(c.args, 'p') || hasLong(c.args, "--eval", "--print") {
		return unknownV("node_inline_code")
	}
	if hasLong(c.args, "--version", "-v") {
		return safeV()
	}
	if c.scriptInWorkspace(c.args, ".js", ".mjs", ".cjs", ".ts") && c.argsStayInWorkspace(c.args) {
		return safeV()
	}
	return unknownV("node_script")
}

func workspaceToolHandler(c *cc) verdict {
	if c.argsStayInWorkspace(c.args) {
		return safeV()
	}
	return unknownV("tool_path_outside_workspace")
}
