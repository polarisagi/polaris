//go:build ignore

// task_state_lint 检查 tasks 表的 CAS 状态转移 SQL 是否符合 inv_M8_03 状态机定义（F-5）。
//
// inv_M8_03 合法状态转移：
//   pending  -> claimed
//   claimed  -> running | failed
//   running  -> done | failed
//
// 违规：允许 claimed 直接跳到 done（绕过 running 导致认领超时乱序）。
//
// 使用：
//	go run tools/task_state_lint.go
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

var errCount int
var queryCount int

func main() {
	targetDirs := []string{"internal/execute/orchestrator", "internal/store/repo"}
	fset := token.NewFileSet()

	for _, dir := range targetDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkFile(fset, path)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "task_state_lint: walk %s: %v\n", dir, err)
			os.Exit(2)
		}
	}

	fmt.Printf("task_state_lint: scanned %d task state update query(ies)\n", queryCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "task_state_lint: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("task_state_lint: PASS")
}

func checkFile(fset *token.FileSet, path string) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		str := strings.ToLower(lit.Value)

		// 检查 UPDATE tasks SET status=...
		if strings.Contains(str, "update tasks") && strings.Contains(str, "status") {
			queryCount++
			pos := fset.Position(lit.Pos())

			// 如果是更新 status 为 statusDone/statusDone 的 SQL，检查 WHERE 条件是否包含 claimed (而非 running)
			if strings.Contains(str, "statusdone") || strings.Contains(str, "status=") || strings.Contains(str, "status =") {
				// 检查同一 SQL 字符串中是否有从 claimed 跳到 done
				if (strings.Contains(str, "statusdone") || strings.Contains(str, "status=?")) &&
					strings.Contains(str, "statusclaimed") && !strings.Contains(str, "statusrunning") {
					fmt.Printf("%s:%d: SQL 尝试从 claimed 状态直接更新为 done 状态，跳过 running（违反 inv_M8_03 F-5）\n",
						path, pos.Line)
					errCount++
				}
			}
		}
		return true
	})
}
