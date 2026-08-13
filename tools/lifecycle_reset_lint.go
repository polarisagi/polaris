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

func main() {
	fset := token.NewFileSet()
	var goFiles []string
	err := filepath.Walk("internal", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Walk failed: %v\n", err)
		os.Exit(2)
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
										fmt.Fprintf(os.Stderr, "%s:%d: !ok 分支以 return nil 结尾，将迎来弹道第一个源就吴杀全部协程（违反 L-09）\n", pos.Filename, pos.Line)
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
