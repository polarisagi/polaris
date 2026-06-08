//go:build ignore

// sync_doc_toc 自动刷新 docs/arch/*.md 文件头的 §跳读 行号。
//
// 设计：
//   - 扫描 ^## <id>. <title> headers 建立 id→line 映射
//   - 解析 `> **§跳读**: id:line? title / id:line? title / ...` 行
//   - 保留人工策展的 title，刷新或注入 line number
//   - 子节锚（无对应 ## header）保持不动
//   - 占位符 `id:title`（无行号）自动注入行号
//
// 用法:
//
//	go run scripts/sync_doc_toc.go              # 重写所有 docs/arch/*.md
//	go run scripts/sync_doc_toc.go -check       # 只校验，drift 时退出非零（CI 用）
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const tocPrefix = "**§跳读**:"

var (
	// 匹配 `## <id>. <title>`；id ∈ {数字, 数字-bis/-ter/-quater, 数字.数字}
	headerRe = regexp.MustCompile(`^## ([0-9]+(?:-bis|-ter|-quater)?(?:\.\d+)?)\.\s+(.+)$`)
	// 占位符尾缀：「（行号 docs-sync 后补）」
	pendingTailRe = regexp.MustCompile(`（行号 docs-sync 后补）\s*$`)
	// 单 entry 形如 `id:NNN title` 或 `id:title`
	entryLineRe = regexp.MustCompile(`^(\d+(?:-bis|-ter|-quater)?(?:\.\d+)?):(.+)$`)
	// 提取 entry 中可选的前导整数行号
	leadingNumRe = regexp.MustCompile(`^(\d+)\s+(.+)$`)
)

func main() {
	check := flag.Bool("check", false, "only verify; exit non-zero if drift detected")
	root := flag.String("root", "docs/arch", "docs root")
	flag.Parse()

	files, err := collectFiles(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "collect: %v\n", err)
		os.Exit(2)
	}

	drift := false
	for _, f := range files {
		changed, err := syncFile(f, *check)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			os.Exit(2)
		}
		if changed {
			drift = true
			fmt.Printf("%s: §跳读 %s\n", f, ifStr(*check, "drift", "synced"))
		}
	}

	if *check && drift {
		fmt.Fprintln(os.Stderr, "drift detected; run `make docs-sync`")
		os.Exit(1)
	}
}

func collectFiles(root string) ([]string, error) {
	pats := []string{"M*.md", "ARCHITECTURE.md"}
	var out []string
	for _, p := range pats {
		matches, err := filepath.Glob(filepath.Join(root, p))
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
	}
	return out, nil
}

// syncFile 重写单个 markdown 文件的 §跳读 行。返回是否有改动。
func syncFile(path string, dryRun bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(data), "\n")

	// 1. 建 id → line(1-indexed) 映射
	headers := map[string]int{}
	for i, line := range lines {
		if m := headerRe.FindStringSubmatch(line); m != nil {
			headers[m[1]] = i + 1
		}
	}

	// 2. 定位 §跳读 行
	tocIdx := -1
	for i, line := range lines {
		if strings.Contains(line, tocPrefix) {
			tocIdx = i
			break
		}
	}
	if tocIdx == -1 {
		return false, nil // 无 §跳读 行 — 不报错，允许文档不带索引
	}

	orig := lines[tocIdx]
	newLine := rebuildToc(orig, headers)
	if newLine == orig {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	lines[tocIdx] = newLine
	return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// rebuildToc 重建一行 §跳读 文本。保留行首所有前缀 (如 `> `)。
func rebuildToc(line string, headers map[string]int) string {
	prefixEnd := strings.Index(line, tocPrefix)
	if prefixEnd < 0 {
		return line
	}
	head := line[:prefixEnd+len(tocPrefix)]
	body := strings.TrimSpace(line[prefixEnd+len(tocPrefix):])
	body = pendingTailRe.ReplaceAllString(body, "")
	body = strings.TrimSpace(body)

	entries := strings.Split(body, " / ")
	for i, e := range entries {
		entries[i] = rewriteEntry(strings.TrimSpace(e), headers)
	}
	return head + " " + strings.Join(entries, " / ")
}

// rewriteEntry 重写单个 entry。无匹配 header 时原样保留。
func rewriteEntry(entry string, headers map[string]int) string {
	m := entryLineRe.FindStringSubmatch(entry)
	if m == nil {
		return entry // 不符合 `id:rest` 格式 — 保留
	}
	id, rest := m[1], strings.TrimSpace(m[2])

	actualLine, ok := headers[id]
	if !ok {
		return entry // 子节锚或未匹配 — 保留
	}

	// rest 可能形如 "18 状态机" (旧行号 + title) 或 "状态机" (纯 title)
	title := rest
	if mm := leadingNumRe.FindStringSubmatch(rest); mm != nil {
		title = mm[2]
	}
	return fmt.Sprintf("%s:%d %s", id, actualLine, title)
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
