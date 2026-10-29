package policy

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// literalWords 把 shell 词还原为字面字符串；含变量/命令替换的部分以 DynToken 占位。
// $HOME / ${HOME} 在 home 非空时直接代入——"rm -rf $HOME" 必须能被识别为删除家目录。
func literalWords(args []*syntax.Word, home string) ([]string, []bool) {
	words := make([]string, 0, len(args))
	dyn := make([]bool, 0, len(args))
	for _, a := range args {
		s, d := wordString(a, home)
		words = append(words, s)
		dyn = append(dyn, d)
	}
	return words, dyn
}

func wordString(word *syntax.Word, home string) (string, bool) {
	var b strings.Builder
	dyn := false
	for _, p := range word.Parts {
		if appendPart(&b, p, home) {
			dyn = true
		}
	}
	return b.String(), dyn
}

// appendPart 追加一个词片段，返回是否含运行期才能确定的内容。
func appendPart(b *strings.Builder, p syntax.WordPart, home string) bool {
	switch x := p.(type) {
	case *syntax.Lit:
		b.WriteString(unescapeLit(x.Value))
	case *syntax.SglQuoted:
		b.WriteString(x.Value)
	case *syntax.DblQuoted:
		dyn := false
		for _, in := range x.Parts {
			if appendPart(b, in, home) {
				dyn = true
			}
		}
		return dyn
	case *syntax.ParamExp:
		if home != "" && isPlainParam(x, "HOME") {
			b.WriteString(home)
			return false
		}
		b.WriteString(DynToken)
		return true
	default:
		b.WriteString(DynToken)
		return true
	}
	return false
}

func isPlainParam(x *syntax.ParamExp, name string) bool {
	return x.Param != nil && x.Param.Value == name && x.Exp == nil && x.Index == nil &&
		x.Slice == nil && x.Repl == nil && !x.Length && !x.Width && !x.Excl
}

func unescapeLit(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	esc := false
	for _, r := range s {
		if r == '\\' && !esc {
			esc = true
			continue
		}
		esc = false
		b.WriteRune(r)
	}
	return b.String()
}

func hasDyn(s string) bool { return strings.Contains(s, DynToken) }

// baseProg 取程序基名；标准 bin 目录下的绝对路径按基名识别，其余带路径的程序返回空串（按未知处理）。
func baseProg(word string) string {
	if !strings.Contains(word, "/") {
		return word
	}
	switch filepath.Dir(word) {
	case "/bin", "/usr/bin", "/usr/local/bin", "/opt/homebrew/bin", "/sbin", "/usr/sbin":
		return filepath.Base(word)
	}
	return ""
}

// positionals 返回非选项参数（"--" 之后全部算位置参数）。
func positionals(args []string) []string {
	var out []string
	rest := false
	for _, a := range args {
		switch {
		case rest:
			out = append(out, a)
		case a == "--":
			rest = true
		case a == "-" || !strings.HasPrefix(a, "-"):
			out = append(out, a)
		}
	}
	return out
}

// hasShort 判断是否带单字符短选项（支持 -rf 这类合并写法），不看 "--" 之后的参数。
func hasShort(args []string, c byte) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a[1:], c) >= 0 {
			return true
		}
	}
	return false
}

// hasLong 判断是否带长选项（--name 或 --name=value）。
func hasLong(args []string, names ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

// resolveReal 解析符号链接到真实路径：路径不存在时对最近的已存在祖先解析再拼回，
// 保证 macOS /var→/private/var 这类别名在根与目标两侧口径一致。
func resolveReal(p string) string {
	p = filepath.Clean(p)
	rest := ""
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func insideRoots(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
