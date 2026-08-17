//go:build ignore

// bounded_cache_check 断言 bufio.Scanner 的单行上限来自 internal/config 阀值，
// 而不是写死在调用点（L-10）。
//
// 2026-08-17 重写判据。原实现只把第二个实参匹配为 *ast.BasicLit，于是：
//
//	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)  ← BinaryExpr，看不见
//	scanner.Buffer(buf, 102400)                            ← BasicLit，才报
//
// 而全仓 8 个调用点里有 7 个写成前者，唯一写对的那个用的是 config 阀值——也就是说
// 本规则自诞生起从未报过一次红，绿灯完全来自判据的盲区。更糟的是 lint-selftest
// 给它的注入样例恰好是 102400（唯一能被抓的形态），负向验证因此也常绿。
// 判据面的通用形态判定已上收到 lintutil.IsLiteralConstExpr 并有单元测试锁住。
//
// 现判据：
//  1. 接收者必须确认是 bufio.NewScanner(...) 的返回值（消除对任意 .Buffer(a,b) 的误报）；
//  2. 第二个实参若为编译期字面量常量表达式即违规；命名常量、config 阀值、变量均放行。
//
// 棘轮：存量记在 tools/baselines/bounded-cache-baseline.md，只禁增量。
package main

import (
	"go/ast"

	"github.com/polarisagi/polaris/tools/lintutil"
)

func main() {
	r := lintutil.NewReporter("bounded-cache-check", lintutil.LoadBaseline("bounded-cache-baseline.md"))

	lintutil.Walk(r, lintutil.WalkOptions{}, func(f lintutil.File) {
		scanners := scannerIdents(f.AST)
		lintutil.Calls(f.AST, func(call *ast.CallExpr) {
			recv, method, ok := lintutil.SelectorCall(call)
			if !ok || method != "Buffer" || len(call.Args) != 2 || !scanners[recv] {
				return
			}
			r.Anchor()
			if !lintutil.IsLiteralConstExpr(call.Args[1]) {
				return
			}
			r.Violation(f.At(call), "bufio.Scanner.Buffer 的上限写成字面量常量表达式，"+
				"必须引用 internal/config 阀值（违反 L-10）")
		})
	})

	// 锚点自毁断言：判据面是「bufio.Scanner 的 Buffer 调用」。一个都找不到说明扫描根、
	// 接收者识别或调用形态之一已经变了，而不是"仓库很干净"。
	r.RequireAnchors(1, "判据锚在 bufio.NewScanner 返回值上的 .Buffer(buf, max) 调用；"+
		"若 Scanner 的构造方式变了，请同步 scannerIdents")
	r.Done()
}

// scannerIdents 收集文件内由 bufio.NewScanner(...) 赋值的标识符名。
// 只看同文件内的直接赋值：跨文件/跨函数传递的 Scanner 不在判据面内，宁可漏报也不误报——
// L-10 的价值在"新写的调用点别写死"，不在把存量翻个底朝天。
func scannerIdents(f *ast.File) map[string]bool {
	out := map[string]bool{}
	isNewScanner := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		recv, method, ok := lintutil.SelectorCall(call)
		return ok && recv == "bufio" && method == "NewScanner"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range x.Rhs {
				if i < len(x.Lhs) && isNewScanner(rhs) {
					if id, ok := x.Lhs[i].(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range x.Values {
				if i < len(x.Names) && isNewScanner(v) {
					out[x.Names[i].Name] = true
				}
			}
		}
		return true
	})
	return out
}
