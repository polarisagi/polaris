//go:build ignore

// comment_refs 检查 .go 注释里写的仓库路径是否真实存在。
//
// 与 scripts/docs-refs.sh 同源同白名单，只是判定对象从 markdown 反引号换成 Go 注释。
// 拆成独立程序而非塞进 shell：Go 注释里的路径没有反引号定界，需要「前一个字符不是 /
// 也不是单词字符」这类回溯判断来排除 URL 片段与更长标识符的尾巴，BSD grep 无 -P
// 无法可靠表达。
//
// 背景：2026-08-08 复核发现 .go 注释里有 139 处失效路径引用（docs/arch 文件重命名
// 后未同步 41 处、四层布局迁移前的 pkg/* 结构残留、协议层接口拆分后仍指 interfaces.go
// 等）。docs/*.md 侧的同类漂移已由 ADR-0081 的 make docs-refs 拦住并不再复发，
// 代码注释侧却一直没有门控——同一类机械可检的缺陷，靠人工复审必然复发。
//
// 刻意不报的四类（避免误报淹没真实缺陷）：
//  1. 更长记号的尾巴 —— 前一个字符是 '/' 或单词字符（URL 片段 /api/embed、
//     标识符列表 boot_tools/boot_knowledge 里的 "tools/"）；
//  2. Go 符号点记法 —— 任一路径段带点且扩展名不在已知集合（internal/config.SandboxConfig）；
//  3. 白名单条目 —— scripts/docs-refs-allowlist.txt，与文档侧门控共用同一份；
//  4. 非顶层目录开头的相对路径。
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

const allowlistPath = "scripts/docs-refs-allowlist.txt"

var (
	// 顶层目录：引用必须以其中之一开头才纳入检查
	refRe = regexp.MustCompile(`(internal|pkg|cmd|rust|configs|api|scripts|tools|web|docs)/[A-Za-z0-9_./-]*[A-Za-z0-9_-]`)
	// 已知文件扩展名：路径段带点且不在此集合内 → 视为 Go 符号点记法
	knownExt = map[string]bool{
		"go": true, "rs": true, "sql": true, "md": true, "toml": true, "yaml": true,
		"yml": true, "json": true, "txt": true, "tmpl": true, "sh": true, "ps1": true,
		"lock": true, "mod": true, "wasm": true, "proto": true, "cedar": true,
	}
	skipDirs = map[string]bool{
		".git": true, "vendor": true, "node_modules": true, "target": true,
		"dist": true, "build": true, ".idea": true, ".claude": true, ".devdata": true,
	}
)

type finding struct {
	file string
	line int
	ref  string
}

func main() {
	allow, err := loadAllowlist()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取白名单失败: %v\n", err)
		os.Exit(2)
	}

	var findings []finding
	err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		findings = append(findings, scanFile(path, allow)...)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "遍历失败: %v\n", err)
		os.Exit(2)
	}

	if len(findings) == 0 {
		fmt.Println("comment-refs ok（.go 注释无失效路径引用）")
		return
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	fmt.Println("FAIL: .go 注释存在失效路径引用（代码已迁移/删除但注释未跟进）:")
	fmt.Println()
	for _, f := range findings {
		fmt.Printf("  %s:%d: 引用了不存在的路径 -> %s\n", f.file, f.line, f.ref)
	}
	fmt.Println()
	fmt.Println("处理方式二选一：")
	fmt.Println("  1) 路径确实迁移了 → 修正注释里的路径（首选）；")
	fmt.Println("  2) 该路径本就该不存在（注释在记载「已于某日删除/迁移」的历史注记）")
	fmt.Printf("     → 把引用字符串整行加进 %s，并在同行 # 注释里写明缘由。\n", allowlistPath)
	os.Exit(1)
}

func scanFile(path string, allow map[string]bool) []finding {
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
		idx := strings.Index(strings.TrimLeft(line, " \t"), "//")
		if idx != 0 {
			continue // 只看整行注释，避免行尾注释与字符串字面量混淆
		}
		for _, loc := range refRe.FindAllStringIndex(line, -1) {
			// 前一个字符若是 '/' 或单词字符，说明命中的是更长记号的尾巴而非独立路径：
			//   - URL 片段 https://host/api/embed
			//   - 标识符列表 boot_tools/boot_knowledge/boot_agent（"tools/" 只是子串）
			if loc[0] > 0 && isRefTail(line[loc[0]-1]) {
				continue
			}
			ref := strings.TrimRight(line[loc[0]:loc[1]], "./")
			if !strings.Contains(ref, "/") || isSymbolNotation(ref) {
				continue
			}
			if allow[ref] {
				continue
			}
			if _, err := os.Stat(ref); err == nil {
				continue
			}
			out = append(out, finding{path, lineNo, ref})
		}
	}
	return out
}

// isRefTail 报告 c 是否使「紧随其后的匹配」不可能是独立的路径引用。
func isRefTail(c byte) bool {
	return c == '/' || c == '_' || c == '-' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isSymbolNotation 报告 ref 是否为 Go 符号点记法（internal/config.SandboxConfig）
// 而非文件路径：任一路径段带点、且最后一个点之后不是已知扩展名即判定为符号。
func isSymbolNotation(ref string) bool {
	for _, seg := range strings.Split(ref, "/") {
		dot := strings.LastIndex(seg, ".")
		if dot < 0 {
			continue
		}
		if !knownExt[seg[dot+1:]] {
			return true
		}
	}
	return false
}

func loadAllowlist() (map[string]bool, error) {
	allow := map[string]bool{}
	f, err := os.Open(allowlistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return allow, nil
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
			allow[strings.TrimRight(line, "/")] = true
		}
	}
	return allow, sc.Err()
}
