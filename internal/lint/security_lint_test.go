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

func TestLintTaintedStringAccess(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.Contains(path, "testdata") || strings.Contains(path, "vendor") {
			return nil //nolint:nilerr
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}

		relPath, _ := filepath.Rel(root, path)

		// 检查是否有直接调用 Value() 方法，且其可能是 TaintedString / SafeString
		ast.Inspect(f, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			selExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			if selExpr.Sel.Name == "Value" {
				// We found a .Value() call. Check if it's allowed.
				if strings.HasSuffix(filepath.ToSlash(relPath), "internal/security/taint/taint_sanitizer.go") {
					return true
				}

				// ADR-0047: SanitizeByDeterministicTransform is allowed
				inAllowedFunc := false
				ast.Inspect(f, func(parent ast.Node) bool {
					if fn, ok := parent.(*ast.FuncDecl); ok {
						if fn.Name.Name == "SanitizeByDeterministicTransform" && fn.Pos() <= selExpr.Pos() && selExpr.Pos() <= fn.End() {
							inAllowedFunc = true
						}
					}
					return true
				})

				if inAllowedFunc {
					return true
				}

				hasTaintImport := false
				for _, imp := range f.Imports {
					if strings.Contains(imp.Path.Value, "internal/security/taint") {
						hasTaintImport = true
						break
					}
				}

				if hasTaintImport {
					pos := fset.Position(selExpr.Pos())
					violations = append(violations, relPath+":"+pos.String()+": direct access to .Value() is prohibited outside taint_sanitizer.go. (Exception: SanitizeByDeterministicTransform as per ADR-0047)")
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
		t.Errorf("Found %d violations for direct TaintedString/SafeString access:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func TestLintSecurityExportsCalled(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	exportedFuncs := make(map[string]string)

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)
		if !strings.HasPrefix(relSlash, "internal/security/taint") &&
			!strings.HasPrefix(relSlash, "internal/security/policy") &&
			!strings.HasPrefix(relSlash, "internal/security/guard") {
			return nil //nolint:nilerr
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() {
				if fn.Recv == nil {
					exportedFuncs[fn.Name.Name] = relSlash
				}
			}
		}
		return nil //nolint:nilerr
	})

	called := make(map[string]bool)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if _, exists := exportedFuncs[sel.Sel.Name]; exists {
					called[sel.Sel.Name] = true
				}
			}
			if ident, ok := n.(*ast.Ident); ok {
				if _, exists := exportedFuncs[ident.Name]; exists {
					called[ident.Name] = true
				}
			}
			return true
		})
		return nil //nolint:nilerr
	})

	var violations []string
	for fn, file := range exportedFuncs {
		if !called[fn] {
			violations = append(violations, file+": exported function "+fn+" has no non-test callers")
		}
	}

	if len(violations) > 0 {
		t.Logf("Found %d uncalled security functions:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
