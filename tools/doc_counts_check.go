//go:build ignore

// doc_counts_check 校验根 CLAUDE.md 里写死的几个"数量"断言与仓库实际一致。
//
// 背景（2026-08-09）：这类计数在一周内漂了四次——P2 一次性订正三处（internal 模块
// 28→29、schema SQL 33→35、ADR 29→40），随后 P4-0 新增一份 ADR 又让"40 份"当场
// 失真。计数漂移 100% 机械可检，却每次都靠人工通读才发现，完全符合本仓库"可机械化
// 的发现必须转门控，否则下轮必然重现"的既有判断（ADR-0062/0081/0091 同源）。
//
// 处置分两类，本工具只管后者：
//   - **无导航价值的计数直接删**：ADR 份数已从 CLAUDE.md 删除——紧邻的下一段就写着
//     "本文件不维护副本，避免双份索引漂移"，再放一个份数自相矛盾；且 `make docs-refs`
//     的 adr-index 项每次运行都打印实时份数，需要时随手可得。
//   - **有导航价值的计数留下 + 上门控**：模块数/SQL 数/模块级 CLAUDE.md 数三项给
//     AI 冷启动提供规模感，其中"001~024 + 028~038 共 35 个"还承载着解释 025~027
//     跳号的作用，删了会丢信息。故保留原文，由本工具保证其不失真。
//
// 关键设计：**断言句式匹配不到时必须 fail，不能当作"没有该断言"静默放行**。否则
// 有人改写措辞就会让检查器悄悄停止工作，退化成 ADR-0091 说的"门控失效与门控通过
// 在输出上长得一模一样"。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

const claudeMD = "CLAUDE.md"

type claim struct {
	desc    string
	pattern *regexp.Regexp // 必须含且仅含一个数字捕获组
	actual  func() (int, error)
	hint    string
}

func main() {
	raw, err := os.ReadFile(claudeMD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n", claudeMD, err)
		os.Exit(2)
	}
	text := string(raw)

	claims := []claim{
		{
			desc:    "internal 一级模块数",
			pattern: regexp.MustCompile(`([0-9]+) internal module|internal/\s+([0-9]+) 模块`),
			actual:  countDirs("internal"),
			hint:    "新增/删除 internal/<模块>/ 目录后须同步 CLAUDE.md 的模块数（共 2 处）",
		},
		{
			desc:    "protocol/schema DDL 文件数",
			pattern: regexp.MustCompile(`DDL SQL（([0-9]+) 个，SSoT）|共 ([0-9]+) 个 SQL 文件|即得（([0-9]+) 个）`),
			actual:  countGlob("internal/protocol/schema/*.sql"),
			hint:    "新增 schema SQL 后须同步 CLAUDE.md 的文件数（共 3 处），并确认跳号说明仍成立",
		},
		{
			desc:    "带 CLAUDE.md 的模块目录数",
			pattern: regexp.MustCompile(`共 ([0-9]+) 个目录有对应文件`),
			actual:  countModuleClaudeMD,
			hint:    "新增/删除 internal/**/CLAUDE.md 后须同步 CLAUDE.md §模块上下文 的目录数与枚举名单",
		},
	}

	var problems []string
	for _, c := range claims {
		matches := c.pattern.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			problems = append(problems, fmt.Sprintf(
				"「%s」的断言句式在 %s 中一处都没匹配到——要么该断言被删了（请同步删除本工具中的对应 claim），"+
					"要么措辞被改动导致检查器失效。检查器不得在匹配不到时静默放行。", c.desc, claudeMD))
			continue
		}
		want, err := c.actual()
		if err != nil {
			problems = append(problems, fmt.Sprintf("统计「%s」实际值失败: %v", c.desc, err))
			continue
		}
		for _, m := range matches {
			got, ok := firstNumber(m)
			if !ok {
				continue
			}
			if got != want {
				problems = append(problems, fmt.Sprintf(
					"「%s」：%s 写的是 %d，实际是 %d —— %s", c.desc, claudeMD, got, want, c.hint))
			}
		}
	}

	if len(problems) > 0 {
		fmt.Println("FAIL: CLAUDE.md 计数断言与仓库实际不符:")
		fmt.Println()
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
		os.Exit(1)
	}
	fmt.Printf("doc-counts ok（%d 项计数断言与仓库实际一致）\n", len(claims))
}

// firstNumber 取捕获组里第一个非空值——多分支正则每次只命中一个分支，其余为空。
func firstNumber(m []string) (int, bool) {
	for _, g := range m[1:] {
		if g == "" {
			continue
		}
		n, err := strconv.Atoi(g)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func countDirs(dir string) func() (int, error) {
	return func() (int, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, e := range entries {
			if e.IsDir() {
				n++
			}
		}
		return n, nil
	}
}

func countGlob(pattern string) func() (int, error) {
	return func() (int, error) {
		m, err := filepath.Glob(pattern)
		return len(m), err
	}
}

func countModuleClaudeMD() (int, error) {
	n := 0
	err := filepath.Walk("internal", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if !info.IsDir() && info.Name() == "CLAUDE.md" {
			n++
		}
		return nil
	})
	return n, err
}
