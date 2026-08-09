//go:build ignore

// adr_index_check 校验 docs/arch/decisions/ 的编号体系自洽性。
//
// 背景（2026-08-09，PS-011/PS-012 复盘）：P5 人工逐份过查 ADR 时发现三类问题，
// 全部 100% 机械可检，却是靠人肉通读才发现的——
//   - `ADR-0051-eval-harness-isolation.md` 存在但索引表里没有它那一行（漏登记）；
//   - 编号 0051 同时出现在「索引」表与「已删除」表，即编号被复用（违反 README
//     顶部与根 CLAUDE.md「三项不可变」之一：ADR 编号一经分配不复用）；
//   - 改号/重命名时文件名与正文 H1 标题的编号可能对不上。
//
// 按本仓库一贯思路（ADR-0062 deadcode、ADR-0081 docs-refs、ADR-0091 覆盖面优先）：
// 人工审查发现的可机械化问题必须转成门控，否则下一轮必然重现。本工具即该转化。
//
// 刻意不做的检查：索引表标题/状态措辞与正文是否逐字一致。README 索引是**摘要**，
// 缩写属正常（如正文"架构文档结构治理 — 失效路径引用 CI 门控 + 拆分提案暂缓"在
// 索引里简写），做字符串比对必然大量误报，误报淹没真实缺陷比漏报更致命（沿用
// ADR-0081 决策一同款判断）。措辞精度问题留给 LLM 审查轮。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	decisionsDir = "docs/arch/decisions"
	readmePath   = "docs/arch/decisions/README.md"
)

var (
	// 文件名：ADR-0093-eval-harness-isolation.md（排除 ADR-template.md）
	fileRe = regexp.MustCompile(`^ADR-([0-9]{4})-.*\.md$`)
	// 正文首行：# ADR-0093: xxx  或  # ADR-0093：xxx
	titleRe = regexp.MustCompile(`^#\s+ADR-([0-9]{4})\s*[:：]`)
	// 表格行首格：| 0093 | ...
	rowRe = regexp.MustCompile(`^\|\s*([0-9]{4})\s*\|`)
)

func main() {
	files, err := collectFiles()
	if err != nil {
		fail("扫描 %s 失败: %v", decisionsDir, err)
	}
	index, deleted, err := parseReadme()
	if err != nil {
		fail("解析 %s 失败: %v", readmePath, err)
	}

	var problems []string

	// A. 存在文件但索引表无对应行（本次实际发生过：ADR-0051 漏登记）
	for _, n := range sortedKeys(files) {
		if !index[n] {
			problems = append(problems, fmt.Sprintf(
				"编号 %s 有文件 %s，但「索引」表中没有对应行——新增 ADR 后须同步登记索引", n, files[n]))
		}
	}

	// B. 索引表有行但无对应文件
	for _, n := range sortedKeys(index) {
		if _, ok := files[n]; !ok {
			problems = append(problems, fmt.Sprintf(
				"编号 %s 在「索引」表中，但 %s/ 下无对应 ADR-%s-*.md 文件", n, decisionsDir, n))
		}
	}

	// C. 同一编号同时出现在「索引」与「已删除」= 编号被复用（不可变规则）
	for _, n := range sortedKeys(index) {
		if deleted[n] {
			problems = append(problems, fmt.Sprintf(
				"编号 %s 同时出现在「索引」表与「已删除」表——编号一经分配不复用（README 顶部 / CLAUDE.md 三项不可变），"+
					"应由后来占用者改用新编号，而非改动已删除记录", n))
		}
	}

	// D. 「已删除」表中的编号不应仍有存活文件
	for _, n := range sortedKeys(deleted) {
		if f, ok := files[n]; ok && !index[n] {
			problems = append(problems, fmt.Sprintf(
				"编号 %s 登记为已删除，却仍存在文件 %s", n, f))
		}
	}

	// E. 文件名编号与正文 H1 标题编号一致（改号时最容易漏改标题）
	for _, n := range sortedKeys(files) {
		path := filepath.Join(decisionsDir, files[n])
		got, err := firstTitleNumber(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("读取 %s 失败: %v", files[n], err))
			continue
		}
		if got == "" {
			problems = append(problems, fmt.Sprintf(
				"%s 首行不是 `# ADR-NNNN: 标题` 格式，无法核对编号", files[n]))
			continue
		}
		if got != n {
			problems = append(problems, fmt.Sprintf(
				"%s 文件名编号是 %s，正文标题写的却是 ADR-%s——改号时漏改标题", files[n], n, got))
		}
	}

	if len(problems) > 0 {
		fmt.Println("FAIL: ADR 编号体系不自洽:")
		fmt.Println()
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
		os.Exit(1)
	}
	fmt.Printf("adr-index ok（%d 份 ADR，编号与索引/已删除表自洽，无复用）\n", len(files))
}

func collectFiles() (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(decisionsDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue // ADR-template.md / README.md 等
		}
		if prev, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("编号 %s 有两个文件: %s 与 %s", m[1], prev, e.Name())
		}
		out[m[1]] = e.Name()
	}
	return out, nil
}

// parseReadme 按「## 索引」/「## 已删除」两个小节切分，分别收集表格行首的编号。
func parseReadme() (index, deleted map[string]bool, err error) {
	raw, err := os.ReadFile(readmePath)
	if err != nil {
		return nil, nil, err
	}
	index, deleted = map[string]bool{}, map[string]bool{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "## 索引"):
			section = "index"
			continue
		case strings.HasPrefix(line, "## 已删除"):
			section = "deleted"
			continue
		case strings.HasPrefix(line, "## "):
			section = ""
			continue
		}
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch section {
		case "index":
			index[m[1]] = true
		case "deleted":
			deleted[m[1]] = true
		}
	}
	return index, deleted, nil
}

func firstTitleNumber(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := titleRe.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
		return "", nil // 首个非空行不是标准标题行
	}
	return "", nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
