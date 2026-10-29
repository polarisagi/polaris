package lint_test

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// TestNoSilentDBFailure (ADR-0094 决策四) AST 扫描 internal/store/ 和 internal/gateway/ 下的 SQL 操作，
// 确保 DB/Tx/Stmt 的 Exec/Query/QueryRow 调用未被静默忽略。
func TestNoSilentDBFailure(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	for _, targetDir := range []string{"internal/store", "internal/gateway"} {
		walkGoFilesUnder(t, root, targetDir, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
			if strings.HasSuffix(relPath, "_test.go") {
				return
			}
			ast.Inspect(f, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				// 检查左值全为 _ 的赋值
				allBlank := len(assign.Lhs) > 0
				for _, lhs := range assign.Lhs {
					if ident, ok := lhs.(*ast.Ident); !ok || ident.Name != "_" {
						allBlank = false
						break
					}
				}
				if !allBlank {
					return true
				}

				// 检查右值是否为 DB Exec/Query/QueryRow/Scan
				for _, rhs := range assign.Rhs {
					if call, ok := rhs.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
							method := sel.Sel.Name
							if method == "Exec" || method == "ExecContext" || method == "Query" || method == "QueryContext" {
								pos := fset.Position(assign.Pos())
								violations = append(violations, violation{
									relPath: relPath,
									line:    pos.Line,
									detail:  "silent DB execution error discard found on " + method,
								})
							}
						}
					}
				}
				return true
			})
		})
	}

	for _, v := range violations {
		t.Errorf("NoSilentDBFailure VIOLATED: %s:%d %s", v.relPath, v.line, v.detail)
	}
}
