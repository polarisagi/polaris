//go:build ignore

// panic_lint 扫描 internal/ 与 pkg/ 下违反 [E1] 的框架层 panic Call（F-12）。
//
// 规则 [E1]：Panic 仅允许在 init() 与 cmd/polaris/ 进程入口。
// 棘轮机制：存量 15 处不可恢复加密/安全/构造 panic 记录在 local_playground/reports/panic-baseline.md，
//
//	本门控阻断任何**新增**的框架层 panic 调用。
//
// 使用：
//
//	go run tools/panic_lint.go
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

var errCount int
var panicCount int
var baselineCount int

func main() {
	baselinePath := "local_playground/reports/panic-baseline.md"
	baselineMap, _ := loadBaseline(baselinePath)

	roots := []string{"internal", "pkg"}
	fset := token.NewFileSet()

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkFile(fset, path, baselineMap)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "panic_lint: walk %s: %v\n", root, err)
			os.Exit(2)
		}
	}

	fmt.Printf("panic_lint: scanned %d framework panic call(s) (baseline: %d stock item(s))\n", panicCount, baselineCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "panic_lint: FAIL — %d NEW violation(s) not in baseline\n", errCount)
		os.Exit(1)
	}
	fmt.Println("panic_lint: PASS")
}

func loadBaseline(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	bm := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bm[line] = true
	}
	return bm, scanner.Err()
}

func checkFile(fset *token.FileSet, path string, baseline map[string]bool) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}

		if fn.Name.Name == "init" {
			return false
		}

		ast.Inspect(fn.Body, func(bodyNode ast.Node) bool {
			call, ok := bodyNode.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "panic" {
				return true
			}

			panicCount++

			if len(call.Args) == 1 {
				if argIdent, ok := call.Args[0].(*ast.Ident); ok {
					if argIdent.Name == "r" || argIdent.Name == "err" || argIdent.Name == "rec" {
						return true
					}
				}
			}

			pos := fset.Position(call.Pos())
			key := fmt.Sprintf("%s:%d:%s", path, pos.Line, fn.Name.Name)
			if baseline[key] {
				baselineCount++
				return true
			}

			if fn.Name.IsExported() && strings.HasPrefix(fn.Name.Name, "New") {
				fmt.Printf("%s:%d: 导出构造函数 %q 内禁用 panic，请改为返回 error（违反 fail-closed 规范 L-04）\n",
					path, pos.Line, fn.Name.Name)
			} else {
				fmt.Printf("%s:%d: 在非 init() 框架层函数 %q 内新增 panic() 调用（违反 [E1] 错误处理规范 F-12 与棘轮规则）\n",
					path, pos.Line, fn.Name.Name)
			}
			errCount++
			return true
		})

		return true
	})
}
