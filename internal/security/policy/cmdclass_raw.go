package policy

import (
	"regexp"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

// 原文兜底扫描（ADR-0122 决策四）：语法树看不见的东西——运行期展开出来的路径、解码后执行、内嵌脚本里的删除——
// 用对原文的确定性匹配补一道。命中只产出 CmdCatastrophic（硬限制询问），绝不产出自保护 Deny：
// 正则可被拼接/编码绕过，不能当拒绝边界（HE-2）；Deny 只来自语法树静态解析到的路径。
// 它是尽力而为的第二道网，物理保证仍在沙箱 DenyPaths 与 hardened 档。

const (
	rawSensitiveFragment = "sensitive_fragment"
	rawObfuscatedExec    = "obfuscated_exec"
	rawEmbeddedDestroy   = "embedded_script_destroy"
	rawForkBomb          = "fork_bomb"
)

type rawPatterns struct {
	decodePipe    *regexp.Regexp // 解码/下载输出管道进解释器
	forkBomb      *regexp.Regexp // 语法树解析失败时的 fork bomb 兜底
	scriptDestroy *regexp.Regexp // 内嵌脚本里的删除调用
	scriptHome    *regexp.Regexp // 内嵌脚本里的家目录/根路径片段
}

// compileRawPatterns 预编译一次（R1.3：不用包初始化期可变全局，惰性、只读）。
var compileRawPatterns = sync.OnceValue(func() rawPatterns { //nolint:gochecknoglobals // 惰性编译固定正则，无可变状态
	shell := `(?:/[\w./-]*/)?(?:ba|z|da|k)?sh\b`
	// 其他解释器仅在"从 stdin 读代码"形态命中：python3 -m json.tool 之类带参数的不算
	other := `(?:/[\w./-]*/)?(?:python[\d.]*|perl|ruby|node|php)(?:\s+-)?\s*(?:$|[|;&)\n<])`
	pipeInto := `\|&?\s*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+)?(?:` + shell + `|` + other + `)`
	producer := `(?:base64\s+(?:-d|--decode)\b|xxd\s+-r|openssl\s+(?:enc|base64)\s[^|;&\n]*-d\b|\b(?:curl|wget)\b)`
	return rawPatterns{
		decodePipe: regexp.MustCompile(producer + `[^;&\n]*?` + pipeInto),
		forkBomb:   regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*:\s*\|\s*:\s*&`),
		scriptDestroy: regexp.MustCompile(`rmtree|remove_tree|rm_rf|rm_r\b|os\.(?:remove|unlink|rmdir|removedirs)|` +
			`\bunlink(?:sync)?\b|\brmsync\b|\brmdirsync\b|fs\.(?:rm|rmdir|unlink)\b|rimraf|remove_dir_all|\brm\s+-[a-z]*[rf]`),
		scriptHome: regexp.MustCompile(`~|\$home|\$\{home\}|expanduser|homedir|dir\.home|path\.home|home_dir|%userprofile%|` +
			`['"]/['"]|['"]/\*|/users/|/home/|/root\b`),
	}
})

// rawScan 对原文兜底扫描，返回原因码；空串表示未命中。file 为 nil 表示语法树解析失败，
// 此时只做纯文本匹配、不做"数据 vs 执行"豁免。cat 供应敏感片段。
func rawScan(cmd string, file *syntax.File, env CmdEnv, cat *PathCatalog) string {
	text := cmd
	if file != nil {
		text = blankDataArgs(cmd, file)
		if r := scanCalls(file, env); r != "" {
			return r
		}
	}
	if hitsFragment(text, cat.fragments) {
		return rawSensitiveFragment
	}
	pats := compileRawPatterns()
	low := strings.ToLower(text)
	if pats.decodePipe.MatchString(low) {
		return rawObfuscatedExec
	}
	if file == nil && pats.forkBomb.MatchString(low) {
		return rawForkBomb
	}
	return ""
}

// hitsFragment 大小写不敏感的片段匹配。同时比较"去引号/反斜杠"与"反斜杠转正斜杠"两种归一形态，
// 关闭 ~/.po""laris/vault.key、Login\ Data 这类相邻引号/转义拆分。
func hitsFragment(text string, fragments []string) bool {
	low := strings.ToLower(text)
	dequoted := strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(low)
	slashed := strings.NewReplacer(`'`, "", `"`, "", `\ `, " ", `\`, "/").Replace(low)
	for _, f := range fragments {
		f = strings.ToLower(f)
		if f == "" {
			continue
		}
		if containsAnchored(dequoted, f) || containsAnchored(slashed, strings.ReplaceAll(f, `\`, "/")) {
			return true
		}
	}
	return false
}

// containsAnchored 片段在 s 中以"路径形态"出现：片段自身以 . 或 ~ 开头（.aws/credentials、.polaris/vault.key），
// 或紧邻路径分隔符之后（…/Default/Login Data）。裸词（commit 信息里的 Login Data、echo Cookies）不算。
// 另外以字母数字开头/结尾的片段，其外侧不得紧邻字母数字：'tdata' 不应命中 testdata。
func containsAnchored(s, frag string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], frag)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(frag)
		pathShaped := frag[0] == '.' || frag[0] == '~' || (i > 0 && (s[i-1] == '/' || s[i-1] == '\\'))
		okL := !isAlnum(frag[0]) || i == 0 || !isAlnum(s[i-1])
		okR := !isAlnum(frag[len(frag)-1]) || end == len(s) || !isAlnum(s[end])
		if pathShaped && okL && okR {
			return true
		}
		from = i + 1
	}
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// dataCmds 其参数是"被查找/被打印的文本"而非被执行或被读取的对象。
func dataCmds() map[string]bool {
	return map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "echo": true, "printf": true}
}

// rawInterpreters 能把 stdin 当代码执行的程序。
func rawInterpreters() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{"sh", "bash", "zsh", "dash", "ksh", "python", "python3", "perl", "ruby", "node", "php"} {
		m[n] = true
	}
	return m
}

// stageProg 取一条简单命令实际执行的程序基名，跳过 sudo/env 等前缀与其选项、VAR=x 赋值。
func stageProg(words []string) (string, []string) {
	i := 0
	for i < len(words) {
		w := words[i]
		switch {
		case w == "sudo" || w == "env" || w == "nohup" || w == "command" || w == "exec" || w == "doas":
		case i > 0 && (strings.HasPrefix(w, "-") || strings.Contains(w, "=")):
		default:
			return progBase(w), words[i+1:]
		}
		i++
	}
	return "", nil
}

// progBase 任意目录下的程序取基名（原文扫描宁可多认，不像分类器那样只认标准 bin 目录）。
func progBase(w string) string {
	if i := strings.LastIndex(w, "/"); i >= 0 {
		return w[i+1:]
	}
	return w
}

// readsStdinCode 解释器的参数形态是否"从 stdin 读代码执行"。
func readsStdinCode(prog string, args []string) bool {
	if hasShort(args, 'c') || hasShort(args, 'm') || hasShort(args, 'e') {
		return false
	}
	if prog == "sh" || prog == "bash" || prog == "zsh" || prog == "dash" || prog == "ksh" {
		if hasShort(args, 's') {
			return true
		}
	}
	for _, p := range positionals(args) {
		if p != "-" {
			return false
		}
	}
	return true
}

// consumesStdinAsCode 该命令把 stdin 当代码执行，或当参数（xargs）去操作——上游输出是被执行/被读取的对象。
func consumesStdinAsCode(call *syntax.CallExpr, home string) bool {
	words, _ := literalWords(call.Args, home)
	prog, args := stageProg(words)
	return prog == "xargs" || (rawInterpreters()[prog] && readsStdinCode(prog, args))
}

// pipelineStages 把嵌套的管道 BinaryCmd 摊平成阶段列表。
func pipelineStages(s *syntax.Stmt) []*syntax.Stmt {
	if s == nil {
		return nil
	}
	if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) {
		return append(pipelineStages(b.X), pipelineStages(b.Y)...)
	}
	return []*syntax.Stmt{s}
}

// outputConsumed 输出会被当成代码/路径使用的所有简单命令：管道送进（stdin 型）解释器或 xargs，
// 或位于命令/进程替换之内（echo ~/.polaris/vault.key 的输出就是 cat 的路径参数）。它们的参数不是纯数据。
func outputConsumed(file *syntax.File, home string) map[*syntax.CallExpr]bool {
	piped := map[*syntax.CallExpr]bool{}
	mark := func(root syntax.Node) {
		syntax.Walk(root, func(m syntax.Node) bool {
			if c, ok := m.(*syntax.CallExpr); ok {
				piped[c] = true
			}
			return true
		})
	}
	syntax.Walk(file, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			mark(x)
			return true
		case *syntax.ProcSubst:
			mark(x)
			return true
		}
		b, ok := n.(*syntax.BinaryCmd)
		if !ok || (b.Op != syntax.Pipe && b.Op != syntax.PipeAll) {
			return true
		}
		stages := pipelineStages(&syntax.Stmt{Cmd: b})
		for j := 1; j < len(stages); j++ {
			call, ok := stages[j].Cmd.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 || !consumesStdinAsCode(call, home) {
				continue
			}
			for _, prev := range stages[:j] {
				mark(prev)
			}
		}
		return true
	})
	return piped
}

// blankDataArgs 把 grep/rg/ag/echo/printf 参数里的字面文本抹成空格（保持偏移），
// 实现"数据 vs 执行"豁免：grep -r "rm -rf" . 与 echo "base64 -d | sh" > notes.md 不应命中。
// 含命令/进程替换的参数不抹（替换里的命令会被执行）；输出被消费为代码/路径的命令（outputConsumed）不抹。
func blankDataArgs(cmd string, file *syntax.File) string {
	buf := []byte(cmd)
	piped := outputConsumed(file, "")
	dc := dataCmds()
	syntax.Walk(file, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 || piped[call] {
			return true
		}
		words, _ := literalWords(call.Args[:1], "")
		prog, _ := stageProg(words)
		if !dc[prog] {
			return true
		}
		for _, a := range call.Args[1:] {
			if hasSubstitution(a) {
				continue
			}
			s, e := int(a.Pos().Offset()), int(a.End().Offset())
			if s < 0 || e > len(buf) || s >= e {
				continue
			}
			for i := s; i < e; i++ {
				buf[i] = ' '
			}
		}
		return true
	})
	return string(buf)
}

func hasSubstitution(w *syntax.Word) bool {
	found := false
	syntax.Walk(w, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			found = true
		}
		return !found
	})
	return found
}

// scanCalls 逐个简单命令检查：eval/sh -c 的代码串含运行期展开、解释器吃下载/解码的进程替换、内嵌脚本破坏。
func scanCalls(file *syntax.File, env CmdEnv) string {
	reason := ""
	syntax.Walk(file, func(n syntax.Node) bool {
		if reason != "" {
			return false
		}
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		words, dyn := literalWords(call.Args, env.Home)
		prog, args := stageProg(words)
		reason = scanCall(call, prog, args, words, dyn, env)
		return reason == ""
	})
	return reason
}

func scanCall(call *syntax.CallExpr, prog string, args, words []string, dyn []bool, env CmdEnv) string {
	switch {
	case prog == "eval":
		for i := range words {
			if dyn[i] && i > 0 && !evalArgIsGenerator(call.Args[i], env.Home) {
				return rawObfuscatedExec
			}
		}
	case isShellProg(prog):
		if r := scanShellCall(call, args, words, dyn); r != "" {
			return r
		}
	}
	if code, ok := inlineScript(prog, args); ok && inlineScriptDestroys(code, env.Home) {
		return rawEmbeddedDestroy
	}
	return ""
}

// evalArgIsGenerator eval 的参数恰为 "$(已知生成器 字面量参数)"（单条简单命令、无重定向）。
func evalArgIsGenerator(w *syntax.Word, home string) bool {
	parts := w.Parts
	if len(parts) == 1 {
		if dq, ok := parts[0].(*syntax.DblQuoted); ok {
			parts = dq.Parts
		}
	}
	if len(parts) != 1 {
		return false
	}
	cs, ok := parts[0].(*syntax.CmdSubst)
	if !ok || len(cs.Stmts) != 1 || len(cs.Stmts[0].Redirs) != 0 || cs.Stmts[0].Background {
		return false
	}
	c, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(c.Assigns) != 0 {
		return false
	}
	words, dyn := literalWords(c.Args, home)
	for _, d := range dyn {
		if d {
			return false
		}
	}
	return isEvalGenerator(words)
}

func isShellProg(p string) bool {
	switch p {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

func scanShellCall(call *syntax.CallExpr, args, words []string, dyn []bool) string {
	// sh -c <代码串>：代码串含运行期展开，语法树无从判定
	off := len(words) - len(args)
	for i, a := range args {
		if (a == "-c" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && len(a) <= 4)) &&
			i+1 < len(args) && dyn[off+i+1] {
			return rawObfuscatedExec
		}
	}
	// bash <(curl …)：进程替换里的下载/解码输出被当脚本执行
	for _, a := range call.Args[1:] {
		if procSubstFetches(a) {
			return rawObfuscatedExec
		}
	}
	return ""
}

// procSubstFetches 词中的进程替换内有下载/解码类命令。
func procSubstFetches(a *syntax.Word) bool {
	found := false
	syntax.Walk(a, func(n syntax.Node) bool {
		ps, ok := n.(*syntax.ProcSubst)
		if !ok {
			return !found
		}
		syntax.Walk(ps, func(m syntax.Node) bool {
			if c, ok := m.(*syntax.CallExpr); ok && len(c.Args) > 0 {
				w, _ := literalWords(c.Args, "")
				switch p, _ := stageProg(w); p {
				case "curl", "wget", "base64", "xxd", "openssl":
					found = true
				}
			}
			return !found
		})
		return false
	})
	return found
}

// inlineScript 解释器的内嵌代码串（python -c / node -e / perl -e / ruby -e / php -r），取标志之后的全部词。
func inlineScript(prog string, args []string) (string, bool) {
	switch {
	case strings.HasPrefix(prog, "python"), prog == "node", prog == "perl", prog == "ruby", prog == "php":
	default:
		return "", false
	}
	for i, a := range args {
		switch a {
		case "-c", "-e", "-p", "-r", "--eval", "--print":
			return strings.Join(args[i+1:], " "), true
		}
	}
	return "", false
}

func inlineScriptDestroys(code, home string) bool {
	pats := compileRawPatterns()
	low := strings.ToLower(code)
	if !pats.scriptDestroy.MatchString(low) {
		return false
	}
	if h := strings.ToLower(strings.TrimRight(home, "/")); h != "" && strings.Contains(low, h) {
		return true
	}
	return pats.scriptHome.MatchString(low)
}

// isForkBomb 函数体内有一条管道，两端都在调用该函数自身（:(){ :|:& };: 及改名变体）。
func isForkBomb(fd *syntax.FuncDecl) bool {
	if fd == nil || fd.Name == nil || fd.Body == nil {
		return false
	}
	name := fd.Name.Value
	calls := func(s *syntax.Stmt) bool {
		c, ok := s.Cmd.(*syntax.CallExpr)
		if !ok || len(c.Args) == 0 {
			return false
		}
		w, _ := wordString(c.Args[0], "")
		return w == name
	}
	found := false
	syntax.Walk(fd.Body, func(n syntax.Node) bool {
		if b, ok := n.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) && calls(b.X) && calls(b.Y) {
			found = true
		}
		return !found
	})
	return found
}
