package lint_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintFSMControlFlow(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	// Check if a variable name indicates it might be an LLM raw output string
	isLLMVar := func(name string) bool {
		lower := strings.ToLower(name)
		return lower == "res" || lower == "output" || lower == "llmres" || lower == "reply" || lower == "text"
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}

		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)

		// Only check internal/agent/fsm/ and internal/execute/
		if !strings.HasPrefix(relSlash, "internal/agent/fsm") && !strings.HasPrefix(relSlash, "internal/execute") {
			return nil //nolint:nilerr
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}

		ast.Inspect(f, func(n ast.Node) bool {
			// Check if statements
			if ifStmt, ok := n.(*ast.IfStmt); ok {
				if binExpr, ok := ifStmt.Cond.(*ast.BinaryExpr); ok {
					if binExpr.Op == token.EQL || binExpr.Op == token.NEQ {
						// Check if one side is a string literal and the other is an LLM var
						leftLLM := false
						if ident, ok := binExpr.X.(*ast.Ident); ok && isLLMVar(ident.Name) {
							leftLLM = true
						}
						rightLLM := false
						if ident, ok := binExpr.Y.(*ast.Ident); ok && isLLMVar(ident.Name) {
							rightLLM = true
						}

						leftStr := false
						if lit, ok := binExpr.X.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							leftStr = true
						}
						rightStr := false
						if lit, ok := binExpr.Y.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							rightStr = true
						}

						if (leftLLM && rightStr) || (rightLLM && leftStr) {
							pos := fset.Position(ifStmt.Pos())
							violations = append(violations, relSlash+":"+pos.String()+": do not use raw LLM output string for control flow (GD-14-006)")
						}
					}
				}
			}

			// Check switch statements
			if switchStmt, ok := n.(*ast.SwitchStmt); ok {
				if ident, ok := switchStmt.Tag.(*ast.Ident); ok && isLLMVar(ident.Name) {
					pos := fset.Position(switchStmt.Pos())
					violations = append(violations, relSlash+":"+pos.String()+": do not switch on raw LLM output string (GD-14-006)")
				}
			}

			return true
		})
		return nil //nolint:nilerr
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("Found %d FSM control flow violations:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
