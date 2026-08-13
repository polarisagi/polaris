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
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
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
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Buffer" {
				return true
			}

			// check if it's scanner.Buffer
			// we just heuristically check if the second argument is a basic literal
			if len(call.Args) == 2 {
				_, isLit := call.Args[1].(*ast.BasicLit)
				if isLit {
					pos := fset.Position(call.Pos())
					fmt.Fprintf(os.Stderr, "%s:%d: bufio.Scanner.Buffer 容量为字面量，必须引用 internal/config 阀値（违反 L-10）\n", pos.Filename, pos.Line)
					hasError = true
				}
			}
			return true
		})
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Println("bounded-cache-check ok")
}
