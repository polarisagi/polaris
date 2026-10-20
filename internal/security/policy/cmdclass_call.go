package policy

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// verdict 单个命令处理器的结论。delegated=true 表示内层命令已自行记录（包装器场景），不再重复记段。
type verdict struct {
	cls       CmdClass
	reason    string
	network   bool
	delegated bool
}

// cc 一次简单命令调用的上下文。
type cc struct {
	w     *walker
	prog  string
	words []string // 含命令名
	args  []string // 不含命令名
}

type handler func(c *cc) verdict

func safeV() verdict                     { return verdict{cls: CmdSafe} }
func unknownV(reason string) verdict     { return verdict{cls: CmdUnknown, reason: reason} }
func dangerV(reason string) verdict      { return verdict{cls: CmdDangerous, reason: reason} }
func catastropheV(reason string) verdict { return verdict{cls: CmdCatastrophic, reason: reason} }

// maxCmdDepth 包装器/-c 代码串递归深度上限，超出按未知处理（防构造深层嵌套耗尽栈）。
const maxCmdDepth = 4

func (w *walker) call(c *syntax.CallExpr) {
	for _, a := range c.Args {
		w.scanWord(a)
	}
	for _, as := range c.Assigns {
		w.scanWord(as.Value)
		if as.Name != nil && envOverrideRisky(as.Name.Value) {
			w.mark(CmdUnknown, "env_override")
		}
	}
	if len(c.Args) == 0 {
		return
	}
	words, dyn := literalWords(c.Args, w.env.Home)
	if dyn[0] {
		w.record(words, verdict{cls: CmdUnknown, reason: "dynamic_command"})
		return
	}
	w.simple(words)
}

func envOverrideRisky(name string) bool {
	switch name {
	case "PATH", "IFS", "BASH_ENV", "ENV", "SHELL", "GIT_SSH_COMMAND", "GIT_EXTERNAL_DIFF", "PAGER", "EDITOR":
		return true
	}
	return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
}

// simple 分类一条简单命令（words[0] 为命令名）。
func (w *walker) simple(words []string) {
	if len(words) == 0 {
		return
	}
	if w.depth >= maxCmdDepth {
		w.record(words, unknownV("nested_too_deep"))
		return
	}
	prog := baseProg(words[0])
	if prog == "" {
		prog = venvProg(words[0])
	}
	h := w.tab.handlers[prog]
	if h == nil {
		w.record(words, unknownV("unrecognized_command"))
		return
	}
	w.depth++
	v := h(&cc{w: w, prog: prog, words: words, args: words[1:]})
	w.depth--
	if !v.delegated {
		w.record(words, v)
	}
}

func (w *walker) record(words []string, v verdict) {
	w.segs = append(w.segs, CmdSegment{Words: words, Class: v.cls, Network: v.network})
	if v.network {
		w.network = true
	}
	w.mark(v.cls, v.reason)
}

// inner 递归分类包装器之后的内层命令。
func (c *cc) inner(words []string) verdict {
	if len(words) == 0 {
		return safeV()
	}
	c.w.simple(words)
	return verdict{delegated: true}
}

// pathsState 汇总一组路径参数：任一在根外 → outside；否则任一无法确认 → unknown；否则 inside。
func (c *cc) pathsState(paths []string) pathResult {
	res := pathInside
	for _, p := range paths {
		switch c.w.pathState(p, hasDyn(p)) {
		case pathOutside:
			return pathOutside
		case pathUnknown:
			res = pathUnknown
		}
	}
	return res
}

// writePaths 对"写入这些路径"的命令统一出结论：根内安全，根外危险，无法确认未知。
func (c *cc) writePaths(paths []string) verdict {
	for _, p := range paths {
		if isShellRC(p) {
			return dangerV("modify_shell_rc")
		}
	}
	switch c.pathsState(paths) {
	case pathInside:
		return safeV()
	case pathOutside:
		return dangerV("write_outside_workspace")
	default:
		return unknownV("unresolved_path")
	}
}

// SegmentPrefix 取一段命令用于"记住授权"的最窄前缀词：
//   - 多子命令程序取"程序 + 子命令"（npm/pnpm/yarn run 再带脚本名）；
//   - 其余非危险命令取第一个选项（-x/--x）之前的词，至多 3 个词（至少程序名本身）；
//   - 危险段取整段词精确匹配，避免一条放行规则覆盖同前缀下的其他危险变体。
func SegmentPrefix(words []string, class CmdClass) []string {
	if len(words) == 0 {
		return nil
	}
	for _, w := range words {
		if hasDyn(w) {
			return nil // 含运行期展开的词不可记住
		}
	}
	prog := words[0]
	if class < CmdDangerous && subcommandPrograms()[prog] {
		return subcommandPrefix(words)
	}
	if class >= CmdDangerous {
		return append([]string(nil), words...)
	}
	end := 1
	for end < len(words) && end < 3 && !strings.HasPrefix(words[end], "-") {
		end++
	}
	return append([]string(nil), words[:end]...)
}

func subcommandPrograms() map[string]bool {
	return map[string]bool{
		"git": true, "go": true, "npm": true, "pnpm": true, "yarn": true, "cargo": true, "make": true,
		"docker": true, "kubectl": true, "brew": true, "pip": true, "pip3": true, "uv": true, "bun": true,
	}
}

func subcommandPrefix(words []string) []string {
	idx := -1
	for i := 1; i < len(words); i++ {
		if words[i] == "-C" || words[i] == "-c" {
			i++ // 选项自带取值（git -C dir / git -c k=v）
			continue
		}
		if !strings.HasPrefix(words[i], "-") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return append([]string(nil), words[:1]...)
	}
	end := idx + 1
	if sub := words[idx]; (sub == "run" || sub == "run-script") && end < len(words) {
		end++ // npm run build：带上脚本名
	}
	return append([]string(nil), words[:end]...)
}

// PrefixCovers 判定 words 是否被前缀规则 prefix 覆盖（逐词精确匹配，git 不覆盖 gitx）。
func PrefixCovers(prefix, words []string) bool {
	if len(prefix) == 0 || len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}

// venvProg 识别工作区虚拟环境里的 pip/python（如 .venv/bin/pip），其余带路径的程序不识别。
func venvProg(word string) string {
	switch base := filepath.Base(word); base {
	case "pip", "pip3", "python", "python3":
		return base
	}
	return ""
}
