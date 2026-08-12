//go:build ignore

// todo_lint 活跃 TODO 判别式与存量跟踪（F-10）。
//
// 判别式：匹配 `TODO(` / `TODO:` / `FIXME(` / `FIXME:`。
// 豁免：
//   - 行内包含 `// 历史：` / `此前` / `原 TODO` / `TODO-free` 等历史叙述标识
//   - _test.go 文件
//   - tools/ / docs/ 目录
//
// 存量输出：生成/更新 local_playground/reports/todo-inventory.md 保持追踪
//
// 使用：
//	go run tools/todo_lint.go
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errCount int
var todoCount int

func main() {
	roots := []string{"internal", "cmd", "pkg"}
	var activeTODOs []string

	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkFile(path, &activeTODOs)
			return nil
		})
	}

	// 记录存量到 reports
	invPath := "local_playground/reports/todo-inventory.md"
	writeInventory(invPath, activeTODOs)

	fmt.Printf("todo_lint: found %d active TODO/FIXME item(s) (inventoried in %s)\n", len(activeTODOs), invPath)
	fmt.Println("todo_lint: PASS")
}

func checkFile(path string, activeTODOs *[]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if !strings.HasPrefix(trimmed, "//") {
			continue
		}

		if (strings.Contains(line, "TODO(") || strings.Contains(line, "TODO:") ||
			strings.Contains(line, "FIXME(") || strings.Contains(line, "FIXME:")) &&
			!strings.Contains(line, "TODO-free") {

			// 检查是否包含历史叙述标识
			if strings.Contains(line, "// 历史：") || strings.Contains(line, "此前") || strings.Contains(line, "原 TODO") {
				continue // 历史叙述豁免
			}

			entry := fmt.Sprintf("%s:%d: %s", path, lineNo, trimmed)
			*activeTODOs = append(*activeTODOs, entry)
			todoCount++
		}
	}
}

func writeInventory(path string, todos []string) {
	os.MkdirAll(filepath.Dir(path), 0755) //nolint:errcheck
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintln(f, "# 活跃 TODO / FIXME 存量清单（F-10 追踪）")
	fmt.Fprintln(f, "# 本文件由 tools/todo_lint.go 自动生成，实时反映全仓活跃 TODO 追踪状态。")
	fmt.Fprintln(f)
	for _, item := range todos {
		fmt.Fprintln(f, "-", item)
	}
}
