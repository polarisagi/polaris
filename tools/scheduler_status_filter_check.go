//go:build ignore

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

func main() {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/automation/queue.go", nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Parse queue.go failed: %v\n", err)
		os.Exit(2)
	}

	found := false
	var scanAndDispatch *ast.FuncDecl

	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "scanAndDispatch" {
			scanAndDispatch = fn
			break
		}
	}

	if scanAndDispatch == nil {
		fmt.Fprintf(os.Stderr, "internal/automation/queue.go: scanAndDispatch not found\n")
		os.Exit(2)
	}

	ast.Inspect(scanAndDispatch.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING && x.Value == `"running"` {
				found = true
			}
		case *ast.IndexExpr:
			if ident, ok := x.X.(*ast.Ident); ok && ident.Name == "inFlight" {
				found = true
			}
		}
		return !found
	})

	if !found {
		fmt.Fprintf(os.Stderr, "internal/automation/queue.go: scanAndDispatch 缺少 running 状态过滤或 inFlight 判定，将导致重复并发调度（违反 L-07）\n")
		os.Exit(1)
	}

	fmt.Println("scheduler-status-filter-check ok")
}
