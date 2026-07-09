package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

//nolint:gocyclo
func main() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	dirs := []string{
		filepath.Join(cwd, "pkg"),
		filepath.Join(cwd, "internal"),
	}

	violations := make(map[string]bool)

	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			relPath, _ := filepath.Rel(cwd, path)

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return nil //nolint:nilerr
			}

			ast.Inspect(f, func(n ast.Node) bool {
				retStmt, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, res := range retStmt.Results {
					if ident, isIdent := res.(*ast.Ident); isIdent && ident.Name == "err" {
						pos := fset.Position(retStmt.Pos())
						key := fmt.Sprintf("%s:%d", relPath, pos.Line)
						violations[key] = true
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			panic(err)
		}
	}

	outPath := filepath.Join(cwd, "internal", "lint", "testdata", "bare_error_return_baseline.json")
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		panic(err)
	}

	b, err := json.MarshalIndent(violations, "", "  ")
	if err != nil {
		panic(err)
	}

	if err := os.WriteFile(outPath, b, 0644); err != nil {
		panic(err)
	}

	fmt.Printf("Generated baseline with %d entries at %s\n", len(violations), outPath)
}
