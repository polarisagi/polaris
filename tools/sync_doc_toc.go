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

// tocPrefix 匹配 §跳读 索引行的前缀。
//
// 现状排查记录：docs/arch/*.md 头部锚点行实际格式统一为 HTML 注释
// `<!-- §跳读: ... -->`（16/16 文件核实一致），不存在任何一个文件使用
// 本常量此前设定的 Markdown 加粗格式 `**§跳读**:`。这意味着 syncFile 里的
// `strings.Contains(line, tocPrefix)` 判定此前对全部文件恒为 false，
// `make docs-sync`/`docs-check` 实质上从未真正扫描过任何一行——是一处
// 与 CLAUDE.md 记录的"注释与代码行为脱节"同类问题的工具期漂移 bug，
// 而不是 docs 内容本身的 drift。修复为匹配实际使用的裸文本前缀
// （不含 Markdown 加粗标记），同时 rebuildToc/syncFile 需处理行尾的
// HTML 注释收尾符 `-->`（见下方 trailingCommentSuffix 处理）。
const tocPrefix = "§跳读:"

// trailingCommentSuffix 是 HTML 注释锚点行的收尾符。重建 entries 前需要先
// 摘掉它，重建后再拼回，否则最后一个 entry 的 title 会被错误拼入 " -->"。
const trailingCommentSuffix = "-->"

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

	// HTML 注释锚点行（`<!-- §跳读: ... -->`）收尾符需先摘掉，避免混入最后一个 entry。
	tail := ""
	if strings.HasSuffix(body, trailingCommentSuffix) {
		body = strings.TrimSpace(strings.TrimSuffix(body, trailingCommentSuffix))
		tail = " " + trailingCommentSuffix
	}

	entries := strings.Split(body, " / ")
	for i, e := range entries {
		entries[i] = rewriteEntry(strings.TrimSpace(e), headers)
	}
	return head + " " + strings.Join(entries, " / ") + tail
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
