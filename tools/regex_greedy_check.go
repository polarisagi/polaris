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

// isScannedPath 判定一行 baseline 文本是否为路径记录（以某个扫描根开头）。
func isScannedPath(line string) bool {
	for _, root := range scanRoots {
		if strings.HasPrefix(line, root+"/") {
			return true
		}
	}
	return false
}

func main() {
	fset := token.NewFileSet()
	var goFiles []string
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
				goFiles = append(goFiles, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Walk %s failed: %v\n", root, err)
			os.Exit(2)
		}
	}

	baselineMap := make(map[string]bool)
	baselineBytes, err := os.ReadFile("tools/baselines/regex-greedy-baseline.md")
	if err == nil {
		lines := strings.Split(string(baselineBytes), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if isScannedPath(line) {
				baselineMap[line] = true
			}
		}
	}

	allowlistMap := make(map[string]bool)
	allowlistBytes, err := os.ReadFile("tools/baselines/regex-greedy-allowlist.txt")
	if err == nil {
		lines := strings.Split(string(allowlistBytes), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "#") && line != "" {
				allowlistMap[line] = true
			}
		}
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
			if !ok {
				return true
			}

			xIdent, ok := sel.X.(*ast.Ident)
			if !ok || xIdent.Name != "regexp" {
				return true
			}

			if sel.Sel.Name != "MustCompile" && sel.Sel.Name != "Compile" {
				return true
			}

			if len(call.Args) < 1 {
				return true
			}

			argLit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || argLit.Kind != token.STRING {
				return true
			}

			val := strings.Trim(argLit.Value, "\"`")
			if strings.Contains(val, "(?s)") && strings.Contains(val, ".*") {
				pos := fset.Position(call.Pos())
				errLine := fmt.Sprintf("%s:%d: 贪婪跨行正则 (?s).* 可能导致匹配过多，建议改用括号计数扫描", pos.Filename, pos.Line)

				if baselineMap[errLine] {
					// skipped by baseline
				} else if allowlistMap[pos.Filename] {
					// skipped by allowlist (entire file)
				} else {
					fmt.Fprintf(os.Stderr, "%s（违反 L-12）\n", errLine)
					hasError = true
				}
			}

			return true
		})
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Println("regex-greedy-check ok")
}
