package policy

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// 命令安全分类器（ADR-0117 决策四）：把一条 shell 命令确定性地分为 安全/未知/危险/灾难性。
// 纯函数 + 表驱动，不调用 LLM、不执行命令；复合命令（&& || ; | 子 shell $() 反引号）逐段分类，
// 取最严一段。解析失败、动态命令名、无法确认路径的参数一律按"未知"处理（fail-closed 到询问，
// 而不是放行）。与 internal/security/classifier 的正则风险分级互补：后者是工具内第二道防线，
// 本分类器决定"要不要先问人"。

// CmdClass 命令分级，数值越大越严。
type CmdClass int

const (
	CmdSafe         CmdClass = iota // 只读查询/工作区内构建测试，直接执行
	CmdUnknown                      // 未识别，询问（可被记住）
	CmdDangerous                    // 危险，询问（污点 >= High 时为硬限制）
	CmdCatastrophic                 // 灾难性（删根/家目录、sudo、mkfs、写块设备），任何档位都询问且不可记住
)

func (c CmdClass) String() string {
	switch c {
	case CmdSafe:
		return "safe"
	case CmdUnknown:
		return "unknown"
	case CmdDangerous:
		return "dangerous"
	default:
		return "catastrophic"
	}
}

// DynToken 占位符：词中含变量/命令替换等运行期才能确定的部分。它不会匹配任何已记住的规则词。
const DynToken = "\x00dyn"

// CmdEnv 分类所需的环境：可信根目录（项目工作目录 + workspace）、家目录、起始 cwd。
type CmdEnv struct {
	Roots []string
	Home  string
	Cwd   string // 为空取 Roots[0]
}

// CmdSegment 一条简单命令（一个 CallExpr）的分类结果，供记住授权按段匹配。
type CmdSegment struct {
	Words   []string
	Class   CmdClass
	Network bool
}

// CmdVerdict 整条命令的分类结果。
type CmdVerdict struct {
	Class    CmdClass
	Reason   string // 最严一段的原因代码
	Network  bool   // 含联网命令（数据外泄硬限制用）
	Segments []CmdSegment
}

// ClassifyCommand 对 cmd 分级。
func ClassifyCommand(cmd string, env CmdEnv) CmdVerdict {
	w := newWalker(env)
	file, err := parseBash(cmd)
	if err != nil || len(file.Stmts) == 0 {
		return CmdVerdict{Class: CmdUnknown, Reason: "unparsable_command"}
	}
	w.stmts(file.Stmts)
	return CmdVerdict{Class: w.worst, Reason: w.reason, Network: w.network, Segments: w.segs}
}

type walker struct {
	env     CmdEnv
	roots   []string
	cwd     string // 空 = 未知
	worst   CmdClass
	reason  string
	network bool
	segs    []CmdSegment
	depth   int
	tab     cmdTable
}

func newWalker(env CmdEnv) *walker {
	w := &walker{env: env, tab: newCmdTable()}
	for _, r := range env.Roots {
		if r != "" {
			w.roots = append(w.roots, resolveReal(r))
		}
	}
	cwd := env.Cwd
	if cwd == "" && len(env.Roots) > 0 {
		cwd = env.Roots[0]
	}
	if cwd != "" {
		w.cwd = resolveReal(cwd)
	}
	return w
}

func (w *walker) mark(c CmdClass, reason string) {
	if c > w.worst || (c == w.worst && w.reason == "") {
		w.worst, w.reason = c, reason
	}
}

func (w *walker) stmts(list []*syntax.Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

func (w *walker) stmt(s *syntax.Stmt) {
	if s == nil {
		return
	}
	for _, r := range s.Redirs {
		w.redirect(r)
	}
	w.command(s.Cmd)
}

func (w *walker) command(cmd syntax.Command) {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		w.call(c)
	case *syntax.BinaryCmd:
		w.binary(c)
	case *syntax.Subshell:
		saved := w.cwd
		w.stmts(c.Stmts)
		w.cwd = saved
	case *syntax.Block:
		w.stmts(c.Stmts)
	case *syntax.TimeClause:
		w.stmt(c.Stmt)
	case *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause:
		w.compound(c)
	case *syntax.TestClause, *syntax.ArithmCmd, nil:
		// 条件/算术表达式本身无副作用
	default:
		w.mark(CmdUnknown, "unsupported_shell_construct")
	}
}

func (w *walker) compound(cmd syntax.Command) {
	switch c := cmd.(type) {
	case *syntax.IfClause:
		for ic := c; ic != nil; ic = ic.Else {
			w.stmts(ic.Cond)
			w.stmts(ic.Then)
		}
	case *syntax.WhileClause:
		w.stmts(c.Cond)
		w.stmts(c.Do)
	case *syntax.ForClause:
		if it, ok := c.Loop.(*syntax.WordIter); ok {
			for _, word := range it.Items {
				w.scanWord(word)
			}
		}
		w.stmts(c.Do)
	case *syntax.CaseClause:
		w.scanWord(c.Word)
		for _, item := range c.Items {
			w.stmts(item.Stmts)
		}
	}
}

// binary 处理 && || | |&。管道右端是 shell/解释器且读 stdin（curl|sh 形态）一律危险。
func (w *walker) binary(b *syntax.BinaryCmd) {
	w.stmt(b.X)
	if b.Op == syntax.Pipe || b.Op == syntax.PipeAll {
		w.checkPipeToInterpreter(b.Y)
	}
	w.stmt(b.Y)
}

func (w *walker) checkPipeToInterpreter(s *syntax.Stmt) {
	if s == nil {
		return
	}
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	words, _ := literalWords(call.Args, w.env.Home)
	prog := baseProg(words[0])
	if !w.tab.stdinInterpreters[prog] {
		return
	}
	// 带脚本文件或 -c 代码串的不是"读 stdin 执行"；其余视为管道执行
	if len(positionals(words[1:])) == 0 && !hasShort(words[1:], 'c') {
		w.mark(CmdDangerous, "pipe_to_interpreter")
	}
}

// scanWord 扫描词中的命令替换/进程替换，其内部命令同样参与分类。
func (w *walker) scanWord(word *syntax.Word) {
	if word == nil {
		return
	}
	for _, p := range word.Parts {
		w.scanPart(p)
	}
}

func (w *walker) scanPart(p syntax.WordPart) {
	switch x := p.(type) {
	case *syntax.CmdSubst:
		saved := w.cwd
		w.stmts(x.Stmts)
		w.cwd = saved
	case *syntax.ProcSubst:
		w.stmts(x.Stmts)
	case *syntax.DblQuoted:
		for _, in := range x.Parts {
			w.scanPart(in)
		}
	}
}

func (w *walker) redirect(r *syntax.Redirect) {
	if r == nil || r.Word == nil {
		return
	}
	w.scanWord(r.Word)
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll, syntax.RdrInOut:
	case syntax.DplOut:
		if s, dyn := wordString(r.Word, w.env.Home); !dyn && (s == "-" || isAllDigits(s)) {
			return
		}
	default:
		return
	}
	w.writeTarget(r.Word)
}

// writeTarget 判定重定向写入目标：/dev/null 等放行，工作区外危险，无法确认未知。
func (w *walker) writeTarget(word *syntax.Word) {
	s, dyn := wordString(word, w.env.Home)
	if dyn {
		w.mark(CmdUnknown, "redirect_dynamic_target")
		return
	}
	switch s {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty":
		return
	}
	if isShellRC(s) {
		w.mark(CmdDangerous, "modify_shell_rc")
		return
	}
	switch w.pathState(s, false) {
	case pathInside:
	case pathOutside:
		w.mark(CmdDangerous, "write_outside_workspace")
	default:
		w.mark(CmdUnknown, "redirect_unresolved_target")
	}
}

type pathResult int

const (
	pathUnknown pathResult = iota
	pathInside
	pathOutside
)

// pathState 判定 p 是否位于可信根内。dyn=true 表示含运行期展开，无法确认。
func (w *walker) pathState(p string, dyn bool) pathResult {
	if dyn || p == "" || strings.Contains(p, DynToken) {
		return pathUnknown
	}
	abs, ok := absPath(p, w.cwd, w.env.Home)
	if !ok {
		return pathUnknown
	}
	if insideRoots(resolveReal(abs), w.roots) {
		return pathInside
	}
	return pathOutside
}

func absPath(p, cwd, home string) (string, bool) {
	switch {
	case p == "~":
		return home, home != ""
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:]), home != ""
	case filepath.IsAbs(p):
		return filepath.Clean(p), true
	case cwd == "":
		return "", false
	}
	return filepath.Join(cwd, p), true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isShellRC 判断是否为 shell 启动文件（修改它等于持久化后门）。
func isShellRC(p string) bool {
	base := filepath.Base(p)
	switch base {
	case ".bashrc", ".zshrc", ".profile", ".bash_profile", ".zprofile", ".zshenv", ".zlogin", ".bash_login", ".cshrc", "config.fish":
		return true
	}
	return false
}

func parseBash(code string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(code), "") //nolint:wrapcheck // 调用方只判断是否成功
}
