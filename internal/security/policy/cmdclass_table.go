package policy

import "strings"

// cmdTable 命令处理器表。每次分类现建一份（避免包级可变全局，gochecknoglobals），成本可忽略。
type cmdTable struct {
	handlers          map[string]handler
	stdinInterpreters map[string]bool
}

func newCmdTable() cmdTable {
	t := cmdTable{handlers: map[string]handler{}, stdinInterpreters: map[string]bool{}}
	for _, n := range []string{"sh", "bash", "zsh", "dash", "ksh", "fish", "python", "python3", "perl", "ruby", "node", "php", "lua"} {
		t.stdinInterpreters[n] = true
	}
	t.addReadOnly()
	t.addWorkspaceWrite()
	t.addDestructive()
	t.addNetwork()
	t.addWrappers()
	t.addDevTools()
	t.addGit()
	return t
}

func (t *cmdTable) set(h handler, names ...string) {
	for _, n := range names {
		t.handlers[n] = h
	}
}

func (t *cmdTable) addReadOnly() {
	safe := func(*cc) verdict { return safeV() }
	t.set(safe, "ls", "ll", "la", "dir", "vdir", "cat", "tac", "head", "tail", "wc", "grep", "egrep", "fgrep",
		"ag", "which", "whereis", "pwd", "echo", "printf", "date", "tree", "stat", "file", "du", "df", "uname",
		"whoami", "id", "hostname", "basename", "dirname", "realpath", "readlink", "diff", "cmp", "comm", "cut",
		"tr", "nl", "od", "hexdump", "xxd", "strings", "md5", "md5sum", "shasum", "sha1sum", "sha256sum", "cksum",
		"jq", "column", "fold", "rev", "paste", "join", "expand", "unexpand", "true", "false", "sleep", "test",
		"[", ":", "printenv", "ps", "uptime", "free", "vm_stat", "sw_vers", "arch", "nproc", "seq", "fd", "bat",
		"eza", "exa", "exit", "return", "wait", "type")
	t.set(rgHandler, "rg")
	t.set(sortHandler, "sort")
	t.set(uniqHandler, "uniq")
	t.set(findHandler, "find")
	t.set(sedHandler, "sed")
	t.set(awkHandler, "awk", "gawk", "mawk")
	t.set(cdHandler, "cd", "pushd", "popd")
}

func rgHandler(c *cc) verdict {
	if hasLong(c.args, "--pre", "--pre-glob") {
		return unknownV("rg_preprocessor")
	}
	return safeV()
}

func sortHandler(c *cc) verdict {
	if hasShort(c.args, 'o') || hasLong(c.args, "--output") {
		return unknownV("sort_output_file")
	}
	return safeV()
}

func uniqHandler(c *cc) verdict {
	if len(positionals(c.args)) > 1 {
		return unknownV("uniq_output_file")
	}
	return safeV()
}

func cdHandler(c *cc) verdict {
	pos := positionals(c.args)
	if c.prog != "cd" || len(pos) != 1 || pos[0] == "-" {
		c.w.cwd = ""
		return unknownV("cd_unresolved")
	}
	switch c.w.pathState(pos[0], hasDyn(pos[0])) {
	case pathInside:
		abs, _ := absPath(pos[0], c.w.cwd, c.w.env.Home)
		c.w.cwd = resolveReal(abs)
		return safeV()
	case pathOutside:
		// 进入工作区外目录本身无害，但此后的相对路径无法再按工作区判定
		c.w.cwd = ""
		return unknownV("cd_outside_workspace")
	}
	c.w.cwd = ""
	return unknownV("cd_unresolved")
}

func findHandler(c *cc) verdict {
	del := hasLong(c.args, "-delete")
	if (del || findExecsRemoval(c.args)) && !findNarrowed(c.args) {
		// 无收窄谓词时 find 的起点就是删除范围：按 rm 目标判（ADR-0122 决策三）。
		// 带 -name/-mtime 等收窄谓词的（find . -name '*.pyc' -delete）是日常清理，保持危险级。
		for _, start := range findStarts(c.args) {
			if hasDyn(start) {
				continue
			}
			if r := c.catastrophicTarget(start); r != "" {
				return catastropheV(r)
			}
		}
	}
	if del {
		return dangerV("find_delete")
	}
	for _, f := range []string{"-fprint", "-fprint0", "-fprintf", "-fls"} {
		if hasLong(c.args, f) {
			return unknownV("find_write_file")
		}
	}
	for i, a := range c.args {
		if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" {
			return c.findExec(c.args[i+1:])
		}
	}
	return safeV()
}

// findStarts find 的起点路径：跳过 -H/-L/-P 全局选项，取到第一个表达式词（-xxx ( ! ）为止；缺省为 "."。
func findStarts(args []string) []string {
	i := 0
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P") {
		i++
	}
	var out []string
	for ; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" || a == ")" {
			break
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return []string{"."}
	}
	return out
}

// findNarrowed 表达式里有按名称/时间/大小等收窄匹配范围的谓词。-type 不算：-type f -delete 仍删光全部文件。
func findNarrowed(args []string) bool {
	for _, a := range args {
		switch a {
		case "-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-regex", "-iregex", "-lname", "-ilname",
			"-newer", "-anewer", "-cnewer", "-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin", "-size", "-user",
			"-group", "-perm", "-empty", "-samefile", "-newermt":
			return true
		}
	}
	return false
}

// findExecsRemoval -exec/-execdir/-ok/-okdir 之后的程序是删除类命令。
func findExecsRemoval(args []string) bool {
	for i, a := range args {
		if (a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir") && i+1 < len(args) {
			switch baseProg(args[i+1]) {
			case "rm", "rmdir", "unlink", "shred", "trash":
				return true
			}
		}
	}
	return false
}

// findExec 对 -exec 之后到 ; / + 为止的内层命令递归分类。
func (c *cc) findExec(rest []string) verdict {
	end := len(rest)
	for i, a := range rest {
		if a == ";" || a == "+" {
			end = i
			break
		}
	}
	// 本段自身（find）安全，内层命令独立成段
	c.w.record(c.words, safeV())
	c.inner(rest[:end])
	return verdict{delegated: true}
}

func sedHandler(c *cc) verdict {
	if !hasShort(c.args, 'i') && !hasLong(c.args, "--in-place") {
		return safeV()
	}
	pos := positionals(c.args)
	if !hasShort(c.args, 'e') && !hasShort(c.args, 'f') && len(pos) > 0 {
		pos = pos[1:] // 首个位置参数是脚本
	}
	if len(pos) == 0 {
		return unknownV("sed_inplace_no_file")
	}
	return c.writePaths(pos)
}

func awkHandler(c *cc) verdict {
	for _, a := range c.args {
		if hasDyn(a) {
			return unknownV("awk_dynamic_script")
		}
		for _, bad := range []string{"system(", "getline", "|&", "\"|", "> \"", ">\"", "-i inplace", "@include", "@load"} {
			if strings.Contains(a, bad) {
				return unknownV("awk_exec_or_write")
			}
		}
	}
	return safeV()
}
