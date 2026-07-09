package lint_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLintOutboxEntry(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 排除测试和依赖目录
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.Contains(path, "testdata") || strings.Contains(path, "vendor") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr // ignore parse errors
		}

		ast.Inspect(f, func(n ast.Node) bool {
			// Find protocol.OutboxEntry{ TargetEngine: "literal" }
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}

			// 检查是否为 OutboxEntry
			isOutboxEntry := false
			if sel, ok := cl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "OutboxEntry" {
				isOutboxEntry = true
			} else if ident, ok := cl.Type.(*ast.Ident); ok && ident.Name == "OutboxEntry" {
				isOutboxEntry = true
			}

			if isOutboxEntry {
				for _, elt := range cl.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if keyIdent, ok := kv.Key.(*ast.Ident); ok && keyIdent.Name == "TargetEngine" {
							if _, isLiteral := kv.Value.(*ast.BasicLit); isLiteral {
								pos := fset.Position(kv.Pos())
								relPath, _ := filepath.Rel(root, pos.Filename)
								violations = append(violations, relPath+":"+pos.String()+": TargetEngine should not be a literal string (use protocol.Topic* constants)")
							}
						}
					}
				}
			}
			return true
		})
		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("Found %d violations for bare string TargetEngine in OutboxEntry:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func TestLintTypeAssertions(t *testing.T) {
	root := repoRoot(t)
	var violations []string

	// 匹配 \.\(\*mem(store|retrieval)\. 和 \.\(\*learning\.Engine\) 的断言
	regexAssertion := regexp.MustCompile(`\.\(\*(memstore|memretrieval|learning)\.`)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.Contains(path, "testdata") || strings.Contains(path, "vendor") {
			return nil
		}

		relPath, _ := filepath.Rel(root, path)

		// 允许在 cmd/polaris, internal/memory, internal/learning 内部使用
		if strings.HasPrefix(filepath.ToSlash(relPath), "cmd/polaris/") ||
			strings.HasPrefix(filepath.ToSlash(relPath), "internal/memory/") ||
			strings.HasPrefix(filepath.ToSlash(relPath), "internal/learning/") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr
		}

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if regexAssertion.MatchString(line) {
				violations = append(violations, relPath+":"+string(rune(i+1))+": illegal concrete type assertion outside of cmd/polaris")
			}
		}

		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("Found %d violations for illegal concrete type assertions:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
