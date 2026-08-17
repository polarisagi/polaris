//go:build ignore

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// scanRoots 是本仓所有 Go 源码根。2026-08-17 从单 "internal" 扩到三根：扫描根不一致
// 本身就是一种静默漏检（ADR-0089）。扩根前已实测 cmd/ 与 pkg/ 上 0 新增命中。
var scanRoots = []string{"internal", "cmd", "pkg"}

func main() {
	fset := token.NewFileSet()
	var goFiles []string
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go") {
				goFiles = append(goFiles, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Walk %s failed: %v\n", root, err)
			os.Exit(2)
		}
	}

	hasError := false
	for _, path := range goFiles {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			// Find select statement
			selStmt, ok := n.(*ast.SelectStmt)
			if !ok {
				return true
			}

			for _, comm := range selStmt.Body.List {
				cc, ok := comm.(*ast.CommClause)
				if !ok {
					continue
				}

				// Check for case ev, ok := <-ch:
				assign, ok := cc.Comm.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 2 || assign.Tok != token.DEFINE {
					continue
				}

				okIdent, isIdent := assign.Lhs[1].(*ast.Ident)
				if !isIdent {
					continue
				}

				// Check for if !ok block
				for _, stmt := range cc.Body {
					ifStmt, isIf := stmt.(*ast.IfStmt)
					if !isIf {
						continue
					}

					unary, isUnary := ifStmt.Cond.(*ast.UnaryExpr)
					if !isUnary || unary.Op != token.NOT {
						continue
					}
					condIdent, isCondIdent := unary.X.(*ast.Ident)
					if !isCondIdent || condIdent.Name != okIdent.Name {
						continue
					}

					// Check if last stmt is return nil
					if len(ifStmt.Body.List) > 0 {
						lastStmt := ifStmt.Body.List[len(ifStmt.Body.List)-1]
						if retStmt, isRet := lastStmt.(*ast.ReturnStmt); isRet {
							if len(retStmt.Results) == 1 {
								if retIdent, isId := retStmt.Results[0].(*ast.Ident); isId && retIdent.Name == "nil" {
									// Check nolint
									hasNolint := false
									if f.Comments != nil {
										for _, cg := range f.Comments {
											for _, c := range cg.List {
												if fset.Position(c.Pos()).Line == fset.Position(retStmt.Pos()).Line {
													if strings.Contains(c.Text, "//nolint:lifecycle_reset") {
														hasNolint = true
													}
												}
											}
										}
									}
									if !hasNolint {
										pos := fset.Position(retStmt.Pos())
										// 2026-08-17：原报错文案是一串乱码（「将迎来弹道第一个源就吴杀全部协程」），
										// 读者无从判断该改什么。改为直述判据与修法。
										fmt.Fprintf(os.Stderr, "%s:%d: select 的 !ok 分支直接 return nil——任一源通道关闭即终止整个循环，"+
											"其余仍活跃的源被一并放弃（违反 L-09）。应把该通道置 nil 后 continue，"+
											"确需退出时加 //nolint:lifecycle_reset 并说明理由\n", pos.Filename, pos.Line)
										hasError = true
									}
								}
							}
						}
					}
				}
			}
			return true
		})
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Println("lifecycle-reset-check ok")
}
