//go:build ignore

// must_check_error_lint 扫描核心写操作/外部调用的 error 被 _ = 丢弃行为（F-6）。
//
// 名单：tools/must-check-error-calls.txt
// 豁免：defer 语句、_test.go 文件、Close()/Rollback()
//
// 使用：
//
//	go run tools/must_check_error_lint.go
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
var callCount int
var fileCount int

func main() {
	listPath := "tools/must-check-error-calls.txt"
	calls, err := loadCalls(listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "must_check_error_lint: load %s: %v\n", listPath, err)
		os.Exit(2)
	}

	roots := []string{"internal", "cmd", "pkg"}
	fset := token.NewFileSet()

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "testutil/") {
				return nil
			}
			fileCount++
			checkFile(fset, path, calls)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "must_check_error_lint: walk %s: %v\n", root, err)
			os.Exit(2)
		}
	}

	// 打印「扫了多少文件」而不是「命中多少违规」：扫描规模是判断门控是否在工作的
	// 唯一信号，命中数为 0 既可能是干净也可能是瞎（2026-08-12 本行原先打印 callCount，
	// 恒显示 "scanned 0 critical call(s)"，与空转门控的输出完全一样，误导了一整轮复核）。
	fmt.Printf("must_check_error_lint: scanned %d file(s), %d pattern(s); %d hit(s)\n",
		fileCount, len(calls), callCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "must_check_error_lint: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("must_check_error_lint: PASS")
}

func loadCalls(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var calls []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		calls = append(calls, line)
	}
	return calls, scanner.Err()
}

func checkFile(fset *token.FileSet, path string, calls []string) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		// 跳过 defer 语句内的调用
		if _, ok := n.(*ast.DeferStmt); ok {
			return false
		}

		// 检查赋值语句 `_ = func(...)` 或 `_, _ = func(...)`
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}

		// 检查 LHS 是否全是 blank identifier '_'
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

		// 检查 RHS 的调用函数
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			fnStr := exprToString(call.Fun)

			for _, pattern := range calls {
				if matchCallPattern(fnStr, pattern) {
					// 排除 Close 和 Rollback
					if strings.HasSuffix(fnStr, ".Close") || strings.HasSuffix(fnStr, ".Rollback") {
						continue
					}
					callCount++
					pos := fset.Position(call.Pos())
					fmt.Printf("%s:%d: 关键函数调用 %q 的 error 返回值被 _ 静默丢弃（违反 F-6 错误吞没防御）\n",
						path, pos.Line, fnStr)
					errCount++
				}
			}
		}
		return true
	})
}

func matchCallPattern(fnStr, pattern string) bool {
	if strings.HasPrefix(pattern, ".") {
		return strings.HasSuffix(fnStr, pattern)
	}
	return fnStr == pattern
}

func exprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		return exprToString(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}
