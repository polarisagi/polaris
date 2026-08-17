package lintutil

import (
	"go/ast"
	"go/token"
	"strings"
)

// ExprText 把 a.b.c 形式的选择器链还原成文本；其余形态返回空串。
//
// 存在的理由：本仓门控一律走 go/parser 单文件 AST、不引 go/types（要跑得够快、
// 且不能因为某个包编译不过就整条规则失灵）。没有类型信息时，"同一个接收者"只能
// 靠表达式文本比对，这个函数就是那个比对的唯一实现——各规则各写一份的话，
// 「a.b.c 与 a.b.c 是不是同一个」会在不同规则里给出不同答案。
func ExprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		prefix := ExprText(x.X)
		if prefix == "" {
			return ""
		}
		return prefix + "." + x.Sel.Name
	case *ast.ParenExpr:
		return ExprText(x.X)
	}
	return ""
}

// IsLiteralConstExpr 判定表达式是否为编译期字面量常量：字面量本身，或全部由字面量
// 经算术 / 括号 / 一元运算构成（1024*1024、10*1024*1024、-1 等）。
// 命名常量与选择器（config.CurrentThresholds()....）返回 false。
//
// 只匹配 *ast.BasicLit 是本仓踩过的坑：L-10 的旧判据就只认 BasicLit，而仓库里的
// 实际写法全是 1024*1024 这类 BinaryExpr，该规则因此自诞生起从未报过一次红。
// 凡是「禁止硬编码魔数」类的判据，都应当用这个函数而不是直接类型断言。
func IsLiteralConstExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.INT || x.Kind == token.FLOAT
	case *ast.ParenExpr:
		return IsLiteralConstExpr(x.X)
	case *ast.UnaryExpr:
		return IsLiteralConstExpr(x.X)
	case *ast.BinaryExpr:
		return IsLiteralConstExpr(x.X) && IsLiteralConstExpr(x.Y)
	}
	return false
}

// HasNilGuard 报告函数体内是否存在 `<expr> == nil` 的判定，expr 按 ExprText 比对。
func HasNilGuard(body *ast.BlockStmt, target string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || bin.Op != token.EQL {
			return true
		}
		if id, ok := bin.Y.(*ast.Ident); !ok || id.Name != "nil" {
			return true
		}
		if ExprText(bin.X) == target {
			found = true
		}
		return !found
	})
	return found
}

// NolintLines 收集文件中出现 //nolint:<rule> 的行号（含该行与其下一行，覆盖
// 「注释写在违规行上方」与「写在行尾」两种habits）。
//
// 各规则此前各写一份，且对「上一行还是下一行」的处理互相矛盾。豁免是抑制的一种，
// 必须像基线一样有唯一的判定方式，否则同一个 //nolint 在两条规则下行为不同。
func NolintLines(f File, rule string) map[int]bool {
	marker := "//nolint:" + rule
	out := map[int]bool{}
	for _, group := range f.AST.Comments {
		for _, c := range group.List {
			if !strings.Contains(c.Text, marker) {
				continue
			}
			line := f.Fset.Position(c.Pos()).Line
			out[line] = true
			out[line+1] = true
		}
	}
	return out
}

// FuncDecls 遍历文件顶层函数（含方法），跳过无函数体的声明。
func FuncDecls(f File, fn func(*ast.FuncDecl)) {
	for _, decl := range f.AST.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn(fd)
	}
}

// Calls 遍历节点下的全部调用表达式。
func Calls(n ast.Node, fn func(*ast.CallExpr)) {
	ast.Inspect(n, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			fn(call)
		}
		return true
	})
}

// SelectorCall 若 call 形如 `<recv>.<method>(...)`，返回接收者文本与方法名。
func SelectorCall(call *ast.CallExpr) (recv, method string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	return ExprText(sel.X), sel.Sel.Name, true
}
