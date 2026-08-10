package lint_test

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// Test_inv_NoBareGoroutine 检查不允许出现裸 go 语句调用（必须使用 concurrent.SafeGo）。
func Test_inv_NoBareGoroutine(t *testing.T) {
	root := repoRoot(t)

	var violations []violation
	for _, targetDir := range []string{"internal", "cmd"} {
		walkGoFilesUnder(t, root, targetDir, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
			if strings.HasSuffix(relPath, "_test.go") {
				return
			}

			ast.Inspect(f, func(n ast.Node) bool {
				goStmt, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}

				// 检查是否为 concurrent.SafeGo
				isSafeGo := false
				if callExpr, ok := goStmt.Call.Fun.(*ast.SelectorExpr); ok {
					if ident, ok := callExpr.X.(*ast.Ident); ok {
						if ident.Name == "concurrent" && callExpr.Sel.Name == "SafeGo" {
							isSafeGo = true
						}
					}
				}

				if !isSafeGo {
					pos := fset.Position(goStmt.Pos())
					// 检查注释是否包含 //custom-nolint:bare-goroutine (在 go 语句上方 3 行内)
					hasNolint := false
					for _, cg := range f.Comments {
						for _, c := range cg.List {
							cPos := fset.Position(c.Pos())
							// 注释在 goStmt 之前，且行距 <= 3
							if cPos.Line <= pos.Line && pos.Line-cPos.Line <= 3 {
								if strings.Contains(c.Text, "//custom-nolint:bare-goroutine") {
									hasNolint = true
									break
								}
							}
						}
						if hasNolint {
							break
						}
					}

					if !hasNolint {
						violations = append(violations, violation{
							relPath: relPath,
							line:    pos.Line,
							detail:  "裸 goroutine 调用违反约定，请使用 concurrent.SafeGo(ctx, name, fn) 包裹，或添加 //custom-nolint:bare-goroutine 注释说明理由",
						})
					}
				}
				return true
			})
		})
	}

	for _, v := range violations {
		t.Errorf("inv_NoBareGoroutine VIOLATED: %s:%d %s", v.relPath, v.line, v.detail)
	}
}

// TestNoBackgroundCtxInLongLivedGoroutine (ADR-0094 决策三) 检查 internal/ 下的 SafeGo 调用，
// 禁止在带 time.Ticker 循环的长驻协程中直接传入 context.Background()。
func TestNoBackgroundCtxInLongLivedGoroutine(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkGoFilesUnder(t, root, "internal", nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		if strings.HasSuffix(relPath, "_test.go") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
			if !ok || selExpr.Sel.Name != "SafeGo" {
				return true
			}
			if ident, ok := selExpr.X.(*ast.Ident); !ok || ident.Name != "concurrent" {
				return true
			}
			if len(callExpr.Args) > 0 {
				if bgCall, ok := callExpr.Args[0].(*ast.CallExpr); ok {
					if bgSel, ok := bgCall.Fun.(*ast.SelectorExpr); ok {
						if pkgIdent, ok := bgSel.X.(*ast.Ident); ok && pkgIdent.Name == "context" && bgSel.Sel.Name == "Background" {
							// 检查闭包参数内是否含有 time.NewTicker (长驻循环)
							isLongLivedTickerLoop := false
							if len(callExpr.Args) >= 3 {
								fnArg := callExpr.Args[len(callExpr.Args)-1]
								ast.Inspect(fnArg, func(fnNode ast.Node) bool {
									if sel, ok := fnNode.(*ast.SelectorExpr); ok {
										if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" && sel.Sel.Name == "NewTicker" {
											isLongLivedTickerLoop = true
										}
									}
									return true
								})
							}

							if isLongLivedTickerLoop {
								pos := fset.Position(callExpr.Pos())
								violations = append(violations, violation{
									relPath: relPath,
									line:    pos.Line,
									detail:  "long-lived ticker goroutine uses context.Background() instead of RootContext/parent ctx",
								})
							}
						}
					}
				}
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("NoBackgroundCtxInLongLivedGoroutine VIOLATED: %s:%d %s", v.relPath, v.line, v.detail)
	}
}

// TestTickerLoopHonorsCtxDone (ADR-0094 决策三) AST 检查: 包含 time.NewTicker 的 for 循环体内必须有 <-ctx.Done() case。
func TestTickerLoopHonorsCtxDone(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkGoFilesUnder(t, root, "internal", nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		if strings.HasSuffix(relPath, "_test.go") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			hasTicker := false
			ast.Inspect(fn.Body, func(bn ast.Node) bool {
				if sel, ok := bn.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" && sel.Sel.Name == "NewTicker" {
						hasTicker = true
					}
				}
				return true
			})
			if !hasTicker {
				return true
			}

			hasCtxDone := false
			ast.Inspect(fn.Body, func(bn ast.Node) bool {
				if sel, ok := bn.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "Done" {
						hasCtxDone = true
					}
				}
				return true
			})

			if !hasCtxDone {
				pos := fset.Position(fn.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  "ticker loop in function " + fn.Name.Name + " does not monitor <-ctx.Done()",
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("TickerLoopHonorsCtxDone VIOLATED: %s:%d %s", v.relPath, v.line, v.detail)
	}
}

// TestChannelSendHonorsCtxDone (ADR-0094 决策三) 检查长驻监听器中向 channel 写入的 select 必须包含 <-ctx.Done()。
func TestChannelSendHonorsCtxDone(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkGoFilesUnder(t, root, "internal/knowledge/connector", nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		if strings.HasSuffix(relPath, "_test.go") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			selectStmt, ok := n.(*ast.SelectStmt)
			if !ok || selectStmt.Body == nil {
				return true
			}
			hasChanSend := false
			hasCtxDone := false
			for _, stmt := range selectStmt.Body.List {
				if comm, ok := stmt.(*ast.CommClause); ok {
					if comm.Comm != nil {
						if _, ok := comm.Comm.(*ast.SendStmt); ok {
							hasChanSend = true
						}
						ast.Inspect(comm.Comm, func(cn ast.Node) bool {
							if sel, ok := cn.(*ast.SelectorExpr); ok && sel.Sel.Name == "Done" {
								hasCtxDone = true
							}
							return true
						})
					}
				}
			}
			if hasChanSend && !hasCtxDone {
				pos := fset.Position(selectStmt.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  "channel send in select statement does not include <-ctx.Done() case",
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("ChannelSendHonorsCtxDone VIOLATED: %s:%d %s", v.relPath, v.line, v.detail)
	}
}
