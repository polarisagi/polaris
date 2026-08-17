//go:build ignore

// rows_err_lint 断言每个 `for X.Next()` 循环之后都检查了 `X.Err()`（F-7）。
//
// 漏掉 rows.Err() 时，迭代中途发生的错误（连接断开、扫描失败）表现为「循环正常结束、
// 结果少了几行」——静默数据丢失，且在测试里几乎不可能复现。
//
// 迭代变量名动态提取（rows / mrows / pr / iter …），不硬编码 "rows"：硬编码曾导致
// 漏检（B-8）。基线键含变量名（path:line:varName），改名即失配、需重新登记，
// 这是刻意的——换了迭代变量说明这段被重写过，值得再看一眼。
//
// 棘轮：存量记在 tools/baselines/rows-err-baseline.md，只禁增量。
package main

import (
	"fmt"
	"go/ast"

	"github.com/polarisagi/polaris/tools/lintutil"
)

func main() {
	r := lintutil.NewReporter("rows-err-lint", lintutil.LoadBaseline("rows-err-baseline.md"))

	lintutil.Walk(r, lintutil.WalkOptions{}, func(f lintutil.File) {
		lintutil.FuncDecls(f, func(fn *ast.FuncDecl) {
			checkFunc(r, f, fn)
		})
	})

	r.RequireAnchors(1, "判据锚在 `for <ident>.Next()` 循环上；SQL 迭代若改用别的形态请同步本规则")
	r.Done()
}

// checkFunc 以**函数**为单位配对：循环用的迭代变量必须在同一函数体内某处被调用过 .Err()。
// 不做控制流分析（调用在循环前也算通过）——F-7 要防的是「压根没写」，
// 收紧到「必须在循环后」会把 defer 与 helper 封装误判成违规，过严的门控最终会被整体关掉。
func checkFunc(r *lintutil.Reporter, f lintutil.File, fn *ast.FuncDecl) {
	type nextLoop struct {
		varName string
		node    ast.Node
	}
	var loops []nextLoop
	errChecked := map[string]bool{}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if forStmt, ok := n.(*ast.ForStmt); ok && forStmt.Cond != nil {
			if call, ok := forStmt.Cond.(*ast.CallExpr); ok {
				if recv, method, ok := lintutil.SelectorCall(call); ok && method == "Next" && recv != "" {
					loops = append(loops, nextLoop{varName: recv, node: forStmt})
					r.Anchor()
				}
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if recv, method, ok := lintutil.SelectorCall(call); ok && method == "Err" && recv != "" {
				errChecked[recv] = true
			}
		}
		return true
	})

	for _, l := range loops {
		if errChecked[l.varName] {
			continue
		}
		key := fmt.Sprintf("%s:%s", f.At(l.node), l.varName)
		r.Violation(key, "for %s.Next() 循环缺少 %s.Err() 检查，迭代中途出错会表现为静默少行"+
			"（违反 F-7 数据库迭代安全约束与棘轮规则）", l.varName, l.varName)
	}
}
