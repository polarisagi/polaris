//go:build ignore

// lifecycle_reset_lint 拦截 select 多路接收里「任一源通道关闭即终止整个循环」的写法（L-09）。
//
// 判据：`case ev, ok := <-ch:` 的 `if !ok { ... return nil }` 分支。
// 多路 select 里某个源关闭是常态（订阅取消、上游重建），此时正确做法是把该通道置 nil
// 让它退出 select 的候选集，然后 continue；直接 return nil 会把其余仍活跃的源一并放弃，
// 表现为「重建一次上游，整条消费链静默停摆」。
//
// 确需退出时在 return 行加 //nolint:lifecycle_reset 并说明理由。
//
// 扫描根 2026-08-17 从单 "internal" 扩到全仓三根（ADR-0089）。扩根前已实测 0 新增命中。
package main

import (
	"go/ast"
	"go/token"

	"github.com/polarisagi/polaris/tools/lintutil"
)

func main() {
	r := lintutil.NewReporter("lifecycle-reset-check", lintutil.LoadBaseline("lifecycle-reset-baseline.md"))

	lintutil.Walk(r, lintutil.WalkOptions{NeedComments: true}, func(f lintutil.File) {
		exempt := lintutil.NolintLines(f, "lifecycle_reset")

		ast.Inspect(f.AST, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectStmt)
			if !ok {
				return true
			}
			for _, clause := range sel.Body.List {
				cc, ok := clause.(*ast.CommClause)
				if !ok {
					continue
				}
				okIdent := commaOkIdent(cc.Comm)
				if okIdent == "" {
					continue
				}
				r.Anchor()
				for _, ret := range bailoutReturns(cc.Body, okIdent) {
					if exempt[f.Fset.Position(ret.Pos()).Line] {
						continue
					}
					r.Violation(f.At(ret), "select 的 !ok 分支直接 return nil——任一源通道关闭即终止整个循环，"+
						"其余仍活跃的源被一并放弃（违反 L-09）。应把该通道置 nil 后 continue，"+
						"确需退出时加 //nolint:lifecycle_reset 并说明理由")
				}
			}
			return true
		})
	})

	r.RequireAnchors(1, "判据锚在 select 的 `case v, ok := <-ch:` 形态上；全仓一个都没有属异常")
	r.Done()
}

// commaOkIdent 返回 `case v, ok := <-ch:` 里 ok 变量的名字，非该形态返回空串。
func commaOkIdent(comm ast.Stmt) string {
	assign, ok := comm.(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 {
		return ""
	}
	id, ok := assign.Lhs[1].(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// bailoutReturns 找出 `if !<okIdent> { ...; return nil }` 里那条 return。
func bailoutReturns(body []ast.Stmt, okIdent string) []*ast.ReturnStmt {
	var out []*ast.ReturnStmt
	for _, stmt := range body {
		ifStmt, ok := stmt.(*ast.IfStmt)
		if !ok {
			continue
		}
		unary, ok := ifStmt.Cond.(*ast.UnaryExpr)
		if !ok || unary.Op != token.NOT {
			continue
		}
		if id, ok := unary.X.(*ast.Ident); !ok || id.Name != okIdent {
			continue
		}
		if len(ifStmt.Body.List) == 0 {
			continue
		}
		last, ok := ifStmt.Body.List[len(ifStmt.Body.List)-1].(*ast.ReturnStmt)
		if !ok || len(last.Results) != 1 {
			continue
		}
		if id, ok := last.Results[0].(*ast.Ident); ok && id.Name == "nil" {
			out = append(out, last)
		}
	}
	return out
}
