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
