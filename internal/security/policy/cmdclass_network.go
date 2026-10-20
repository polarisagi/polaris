package policy

import "strings"

func (t *cmdTable) addNetwork() {
	t.set(curlHandler, "curl", "wget", "http", "https", "xh")
	t.set(func(*cc) verdict {
		v := dangerV("remote_access")
		v.network = true
		return v
	}, "ssh", "scp", "sftp", "rsync", "nc", "ncat", "netcat", "telnet", "ftp", "socat")
}

// curl/wget：带请求体/上传/非 GET 方法属于数据外发（危险）；纯 GET 视为未知（询问，可按程序记住）。
func curlHandler(c *cc) verdict {
	if c.sendsData() {
		v := dangerV("network_send_data")
		v.network = true
		return v
	}
	v := unknownV("network_fetch")
	v.network = true
	return v
}

func (c *cc) sendsData() bool {
	if hasShort(c.args, 'd') || hasShort(c.args, 'F') || hasShort(c.args, 'T') {
		return true
	}
	if hasLong(c.args, "--data", "--data-raw", "--data-binary", "--data-urlencode", "--data-ascii", "--form",
		"--form-string", "--upload-file", "--json", "--post-data", "--post-file", "--body-data", "--body-file") {
		return true
	}
	for i, a := range c.args {
		if (a == "-X" || a == "--request" || a == "--method") && i+1 < len(c.args) && !strings.EqualFold(c.args[i+1], "GET") {
			return true
		}
		if strings.HasPrefix(a, "-X") && len(a) > 2 && !strings.EqualFold(a[2:], "GET") {
			return true
		}
		if m, ok := strings.CutPrefix(a, "--request="); ok && !strings.EqualFold(m, "GET") {
			return true
		}
		if m, ok := strings.CutPrefix(a, "--method="); ok && !strings.EqualFold(m, "GET") {
			return true
		}
	}
	return false
}

func (t *cmdTable) addWrappers() {
	t.set(wrapperHandler, "nohup", "nice", "timeout", "time", "command", "stdbuf", "ionice", "caffeinate")
	t.set(envHandler, "env")
	t.set(xargsHandler, "xargs")
	t.set(shellHandler, "sh", "bash", "zsh", "dash", "ksh")
}

// wrapperHandler 跳过包装器自身的选项/数值参数，对内层命令递归分类。
func wrapperHandler(c *cc) verdict {
	if c.prog == "command" && (hasShort(c.args, 'v') || hasShort(c.args, 'V')) {
		return safeV()
	}
	rest := c.args
	for len(rest) > 0 && (strings.HasPrefix(rest[0], "-") || isDurationLike(rest[0], c.prog)) {
		// nice -n 10 / ionice -c 2：选项自带取值
		if (rest[0] == "-n" || rest[0] == "-c" || rest[0] == "-s" || rest[0] == "-k") && len(rest) > 1 {
			rest = rest[2:]
			continue
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return safeV()
	}
	return c.inner(rest)
}

func isDurationLike(s, prog string) bool {
	if prog != "timeout" || s == "" {
		return false
	}
	last := s[len(s)-1]
	return (last >= '0' && last <= '9') || last == 's' || last == 'm' || last == 'h' || last == 'd'
}

func envHandler(c *cc) verdict {
	rest := c.args
	for len(rest) > 0 && (strings.Contains(rest[0], "=") || rest[0] == "-i" || rest[0] == "-0") {
		if strings.HasPrefix(rest[0], "PATH=") || strings.HasPrefix(rest[0], "LD_") {
			return unknownV("env_override")
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return safeV()
	}
	return c.inner(rest)
}

// xargs：被调命令的参数来自 stdin，无法逐个核对，只对"命令本身"分类。
func xargsHandler(c *cc) verdict {
	rest := c.args
	valued := map[string]bool{"-I": true, "-n": true, "-P": true, "-d": true, "-E": true, "-L": true, "-s": true, "-a": true}
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if valued[rest[0]] && len(rest) > 1 {
			rest = rest[2:]
			continue
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return safeV() // 默认 echo
	}
	return c.inner(rest)
}

// shellHandler：sh -c "代码串" 对字面代码串递归分类；执行脚本文件内容无法核对，按未知。
func shellHandler(c *cc) verdict {
	for i, a := range c.args {
		if a == "-c" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && len(a) <= 4) {
			if i+1 >= len(c.args) || hasDyn(c.args[i+1]) {
				return unknownV("shell_dynamic_code")
			}
			return c.nested(c.args[i+1])
		}
	}
	return unknownV("shell_script")
}

// nested 对 -c 代码串重新解析分类，复用当前 cwd 与深度。
func (c *cc) nested(code string) verdict {
	sub := newWalker(c.w.env)
	sub.cwd = c.w.cwd
	sub.depth = c.w.depth
	sub.tab = c.w.tab
	file, err := parseBash(code)
	if err != nil || len(file.Stmts) == 0 {
		return unknownV("shell_unparsable_code")
	}
	sub.stmts(file.Stmts)
	c.w.segs = append(c.w.segs, sub.segs...)
	if sub.network {
		c.w.network = true
	}
	c.w.mark(sub.worst, sub.reason)
	return verdict{delegated: true}
}
