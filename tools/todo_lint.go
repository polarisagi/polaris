//go:build ignore

// todo_lint 活跃 TODO 判别式与存量跟踪（F-10）。
//
// 判别式：匹配 `TODO(` / `TODO:` / `FIXME(` / `FIXME:`。
// 豁免：
//   - 行内包含 `// 历史：` / `此前` / `原 TODO` / `TODO-free` 等历史叙述标识
//   - _test.go 文件
//   - tools/ / docs/ 目录
//
// 判定方式：**棘轮**（ADR-0088 存量债处置边界）。存量基线记于
// tools/baselines/todo-baseline.txt，新增活跃 TODO 一律报红。
//
//	2026-08-12 改为棘轮：原实现只把活跃 TODO 写进 inventory 后无条件 PASS，
//	即「记录而不阻断」。后果实测：同一轮里有人把 4 条本该做「接线/删除/登记」
//	三选一裁决的条目，改成新加 5 条 TODO 注释挂起，活跃 TODO 由 2 涨到 7，
//	门控全程绿灯。一个不会失败的检查不是门控，是报表。
//
// 存量输出：生成/更新 local_playground/reports/todo-inventory.md 保持追踪
//
// 使用：
//
//	go run tools/todo_lint.go
//	go run tools/todo_lint.go -update-baseline   # 有意消化存量后重置基线
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errCount int
var todoCount int

const baselinePath = "tools/baselines/todo-baseline.txt"

func main() {
	var updateBaseline bool
	flag.BoolVar(&updateBaseline, "update-baseline", false, "把当前存量写回基线（仅在有意消化/新增存量并已获批时使用）")
	flag.Parse()

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

	if updateBaseline {
		if err := writeBaseline(activeTODOs); err != nil {
			fmt.Fprintf(os.Stderr, "todo_lint: 写基线失败: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("todo_lint: 基线已更新为 %d 条（%s）\n", len(activeTODOs), baselinePath)
		return
	}

	baseline, err := loadBaseline()
	if err != nil {
		fmt.Fprintf(os.Stderr, "todo_lint: 读取基线 %s 失败: %v\n", baselinePath, err)
		os.Exit(2)
	}

	// 棘轮：只拦增量。按「文件:TODO 正文」为键，行号漂移不误报。
	var added []string
	for _, t := range activeTODOs {
		if !baseline[todoKey(t)] {
			added = append(added, t)
		}
	}

	fmt.Printf("todo_lint: found %d active TODO/FIXME item(s) (baseline: %d, inventoried in %s)\n",
		len(activeTODOs), len(baseline), invPath)
	if len(added) > 0 {
		for _, a := range added {
			fmt.Fprintf(os.Stderr, "todo_lint: 新增活跃 TODO —— %s\n", a)
		}
		fmt.Fprintf(os.Stderr, "todo_lint: FAIL — %d 条新增活跃 TODO 不在基线内。\n"+
			"「写了没接线」的处置只有三种：接线 / 删除 / 登记白名单；加一条 TODO 挂起不是其中之一。\n"+
			"确属需要挂账的基础设施缺口 → 转 ADR 或 ROADMAP 条目，并在代码处留可观测信号，\n"+
			"然后跑 `go run tools/todo_lint.go -update-baseline` 重置基线并在 commit 说明理由。\n",
			len(added))
		os.Exit(1)
	}
	fmt.Println("todo_lint: PASS")
}

// todoKey 取「文件路径 + TODO 正文」作为身份，剔除行号，避免无关改动导致误报。
func todoKey(entry string) string {
	// entry 形如 "- path/to/file.go:32: // TODO(x): 正文"
	s := strings.TrimPrefix(strings.TrimSpace(entry), "- ")
	first := strings.Index(s, ":")
	if first < 0 {
		return s
	}
	rest := s[first+1:]
	second := strings.Index(rest, ":")
	if second < 0 {
		return s
	}
	return strings.TrimSpace(s[:first]) + "|" + strings.TrimSpace(rest[second+1:])
}

func loadBaseline() (map[string]bool, error) {
	set := map[string]bool{}
	f, err := os.Open(baselinePath)
	if os.IsNotExist(err) {
		return set, nil // 基线缺失视为空：任何活跃 TODO 都算新增，fail-closed
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[line] = true
	}
	return set, sc.Err()
}

func writeBaseline(items []string) error {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, todoKey(it))
	}
	sort.Strings(keys)

	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# 活跃 TODO 存量基线（F-10 棘轮，ADR-0088 存量债处置边界）\n")
	b.WriteString("# 由 `go run tools/todo_lint.go -update-baseline` 生成。\n")
	b.WriteString("# 每行格式：<文件路径>|<TODO 正文>（不含行号，避免无关改动误报）。\n")
	b.WriteString("# 新增活跃 TODO 一律报红——挂起不是「写了没接线」的合法处置方式。\n\n")
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("\n")
	}
	return os.WriteFile(baselinePath, []byte(b.String()), 0o600)
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
