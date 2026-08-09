//go:build ignore

// anchor_refs 检查 .go 注释与 docs/*.md 里写的 "M13 §1.2" / "M05-Memory-System.md §3.4"
// 这类模块章节锚点引用，其目标章节在对应 M_X.md 中是否真实存在。
//
// 背景（local_playground/prompt/docs-optimization-plan.md P4-0-2）：2026-08-09 实测
// 全仓 386 处 .go 注释引用 M_X 的 § 章节号，而 scripts/docs-refs.sh + tools/comment_refs.go
// 只校验反引号内的文件路径字面量，对 § 锚点零覆盖——M_X 章节编号/标题改动会导致这 386
// 处引用静默失效且 make docs-refs 照常全绿。本工具补上这一格。
//
// 与 comment_refs.go 的差异（刻意，不是疏漏）：
//  1. 判定对象不是路径字面量而是"模块代号 + § + 章节号"这类锚点记法，需要先从
//     docs/arch 下 M 开头的各模块文件建立"文件 → 合法章节号集合"的索引，再核对
//     每处引用是否命中。
//  2. 本工具首跑即预期存在大量存量漂移（历史注释从未被这类门控扫过），故采用
//     baseline 棘轮模式（ADR-0088 先例）：只对"baseline 中没有、新增的"引用报错，
//     不要求存量清零。baseline 文件 scripts/anchor-refs-baseline.txt 随代码提交，
//     人工确认某条是真实修复后才能把它从 baseline 里删掉（少了才是进步，多了才报错）。
//  3. 章节号记法比路径字面量更不规则（"0-bis"、"8.6"、纯数字、极少数历史遗留记法
//     在现有标题里找不到对应——这本身就是门控要抓的漂移，不是解析 bug），故本工具
//     对"模块代号能否解析到具体文件"从严（解析不出来就跳过，避免误伤 R2.1/XR-03
//     这类非模块引用），对"章节号是否命中"从宽（找不到即报，交给 baseline 兜底）。
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const baselinePath = "scripts/anchor-refs-baseline.txt"

var (
	// 模块代号：M + 两位数字，可选 -bis 后缀（M13 / M13-bis）
	moduleFileRe = regexp.MustCompile(`^(M[0-9]{2}(?:-bis)?)-.*\.md$`)

	// 引用记法：M13 §1.2 / M13-Interface-Scheduler.md §1.2 / M13-bis §8.4
	// 捕获组：1=模块代号（含 -bis），2=可选的完整文件名（含扩展名，此时忽略代号部分
	// 已包含的信息，直接用它定位文件），3=章节号
	refRe = regexp.MustCompile(`(M[0-9]{2}(?:-bis)?)(-[A-Za-z][A-Za-z]*(?:-[A-Za-z]+)*\.md)?\s*§\s*([0-9A-Za-z][0-9A-Za-z.\-]*)`)

	// 标题行章节号：## 8.6 xxx / ### 0-bis. xxx / #### 4.1 xxx
	headingRe = regexp.MustCompile(`^#{2,4}\s+([0-9]+(?:-bis|-ter)?(?:\.[0-9]+)*)\.?\s`)

	skipDirs = map[string]bool{
		".git": true, "vendor": true, "node_modules": true, "target": true,
		"dist": true, "build": true, ".idea": true, ".claude": true, ".devdata": true,
		// local_playground/ 含 bake/（用户手工维护的历史备份，非权威源，CLAUDE.md
		// 与审查提示词均明确排除在文档审计范围外）与大量草稿提示词，不是"活文档"。
		// .go 扫描不受影响（该目录本就没有 .go 源码）。
		"local_playground": true,
	}
)

type finding struct {
	file string
	line int
	ref  string // 归一化后的 "模块代号 §章节号"，用于 baseline 比对
	raw  string // 原始命中文本，用于报告展示
}

func main() {
	moduleFiles, err := buildModuleFileIndex("docs/arch")
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立模块文件索引失败: %v\n", err)
		os.Exit(2)
	}

	headings := map[string]map[string]bool{} // 文件路径 → 合法章节号集合
	for _, path := range moduleFiles {
		set, err := extractHeadings(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n", path, err)
			os.Exit(2)
		}
		headings[path] = set
	}

	baseline, err := loadBaseline()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 baseline 失败: %v\n", err)
		os.Exit(2)
	}

	var findings []finding
	err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if info.IsDir() {
			if skipDirs[info.Name()] || path == "docs/arch/decisions" {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".go"):
			findings = append(findings, scanGoFile(path, moduleFiles, headings)...)
		case strings.HasSuffix(path, ".md") && !strings.HasPrefix(path, "docs/arch/decisions/"):
			findings = append(findings, scanMdFile(path, moduleFiles, headings)...)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "遍历失败: %v\n", err)
		os.Exit(2)
	}

	var newFindings []finding
	for _, f := range findings {
		if !baseline[f.ref] {
			newFindings = append(newFindings, f)
		}
	}

	if len(newFindings) == 0 {
		fmt.Printf("anchor-refs ok（§ 锚点引用无新增失效，baseline 存量 %d 条不重复报）\n", len(baseline))
		return
	}

	sort.Slice(newFindings, func(i, j int) bool {
		if newFindings[i].file != newFindings[j].file {
			return newFindings[i].file < newFindings[j].file
		}
		return newFindings[i].line < newFindings[j].line
	})
	fmt.Println("FAIL: 发现新增的失效 § 章节锚点引用（M_X 章节号/标题已变但引用未跟进）:")
	fmt.Println()
	for _, f := range newFindings {
		fmt.Printf("  %s:%d: %s\n", f.file, f.line, f.raw)
	}
	fmt.Println()
	fmt.Println("处理方式二选一：")
	fmt.Println("  1) 章节确实改了 → 修正引用里的章节号（首选）；")
	fmt.Printf("  2) 该引用本就该失效（历史存量，暂不处理）→ 加进 %s 一行（模块代号 §章节号），\n", baselinePath)
	fmt.Println("     baseline 只许新增不许直接绕过——加入前须能说明缘由，不得批量填充。")
	os.Exit(1)
}

func buildModuleFileIndex(dir string) (map[string]string, error) {
	idx := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := moduleFileRe.FindStringSubmatch(e.Name()); m != nil {
			idx[m[1]] = filepath.Join(dir, e.Name())
		}
	}
	return idx, nil
}

func extractHeadings(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if m := headingRe.FindStringSubmatch(sc.Text()); m != nil {
			set[m[1]] = true
		}
	}
	return set, sc.Err()
}

func scanGoFile(path string, moduleFiles map[string]string, headings map[string]map[string]bool) []finding {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []finding
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		idx := strings.Index(line, "//")
		if idx < 0 {
			continue
		}
		out = append(out, checkLine(path, lineNo, line[idx:], moduleFiles, headings)...)
	}
	return out
}

func scanMdFile(path string, moduleFiles map[string]string, headings map[string]map[string]bool) []finding {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []finding
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		out = append(out, checkLine(path, lineNo, sc.Text(), moduleFiles, headings)...)
	}
	return out
}

func checkLine(path string, lineNo int, text string, moduleFiles map[string]string, headings map[string]map[string]bool) []finding {
	var out []finding
	for _, m := range refRe.FindAllStringSubmatch(text, -1) {
		module := m[1]
		anchor := strings.TrimRight(m[3], ".")
		targetFile, ok := moduleFiles[module]
		if !ok {
			continue // 模块代号解析不出具体文件，不在本工具判定范围内（避免误伤非模块引用）
		}
		// 若引用里带了完整文件名，用它二次确认落在同一文件（不一致本身也是一种漂移，
		// 但归入"模块代号解析不到该文件名对应模块"的边界情形，此处不单独细分，交给
		// module 代号解析结果统一处理，避免规则再分叉）。
		set := headings[targetFile]
		if set[anchor] {
			continue
		}
		out = append(out, finding{
			file: path,
			line: lineNo,
			ref:  module + " §" + anchor,
			raw:  fmt.Sprintf("引用了 %s §%s（%s 中无此章节号）-> %s", module, anchor, filepath.Base(targetFile), strings.TrimSpace(m[0])),
		})
	}
	return out
}

func loadBaseline() (map[string]bool, error) {
	baseline := map[string]bool{}
	f, err := os.Open(baselinePath)
	if err != nil {
		if os.IsNotExist(err) {
			return baseline, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			baseline[line] = true
		}
	}
	return baseline, sc.Err()
}
