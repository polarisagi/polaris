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
//	go run tools/sync_doc_toc.go              # 重写所有 docs/arch/*.md
//	go run tools/sync_doc_toc.go -check       # 只校验，drift 时退出非零（CI 用）
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// docIDPattern 是 header id 与 §跳读 entry id 共用的形状定义（两处必须同源，
// 否则会出现"header 认得出、entry 认不出"的半失效——见下方 2026-08-10 追记）。
//
// 覆盖实测存在的全部形态（`grep -h '^## ' docs/arch/*.md` 核实）：
//
//	1 / 10.1 / 2.6                纯数字与子节号（子节可多级）
//	0-bis / 3-quinquies           拉丁序数后缀（-bis/-ter/-quater/-quinquies/-sexies…）
//	§3-sexies                     带 § 前缀的补录节
//
// 后缀用 `-[a-z]+` 而非枚举：此前枚举到 -quater 为止，M08 新增 `3-quinquies`
// 与 `§3-sexies` 后直接落到规则之外，是同类漂移的复发点。
const docIDPattern = `§?[0-9]+(?:-[a-z]+)?(?:\.[0-9]+)*`

var (
	// headerRe 匹配 `## <id>[.] <title>`。
	//
	// 2026-08-10 追记（GR-12-003/004/005 根因）：原规则要求 id 后**必须**跟 `.`
	// 且 `.` 后必须有空白，后缀只枚举到 `-quater`。实测 docs/arch 下 28 个 header
	// 不满足——`## 10.1 PerformanceDrift`（点后无空格写法，实际是"无尾点"）、
	// `## 8.（已删除）编排拓扑自演化`（尾点后直接接全角括号）、`## 3-quinquies.`、
	// `## §3-sexies`。这些 header 建不进 id→line 映射，rewriteEntry 便把对应
	// entry 当"子节锚"原样保留，于是 `make docs-check` 长年绿灯而 §跳读 行号
	// 持续腐烂（M08 `11.2:318` 实际 366、`8:333` 实际 313；M13 `8.6/8.7/8.8`
	// 连行号都没有）。同时 INDEX.md 据此写下"10.1 无对应 header"的错误断言。
	//
	// 故：尾点改可选、点后空白改可选、后缀改通配。
	headerRe = regexp.MustCompile(`^## (` + docIDPattern + `)\.?\s*(\S.*)$`)
	// 占位符尾缀：「（行号 docs-sync 后补）」
	pendingTailRe = regexp.MustCompile(`（行号 docs-sync 后补）\s*$`)
	// 单 entry 形如 `id:NNN title` 或 `id:title`
	entryLineRe = regexp.MustCompile(`^(` + docIDPattern + `):(.+)$`)
	// 提取 entry 中可选的前导整数行号。
	// 分隔空白用 `\s*` 而非 `\s+`：M08 存在 `3-bis:178(已删除,见ADR-0062)` 这类
	// 旧行号后直接接全角/半角括号的写法，用 `\s+` 会认不出行号，把 `178(已删除…`
	// 整段当 title 保留，刷新后变成 `3-bis:181 178(已删除…` 这种双行号污染。
	leadingNumRe = regexp.MustCompile(`^(\d+)\s*(.+)$`)
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

	// 逐文件收集错误而非首个即退：一次跑完给出全部缺口，否则补一个章节要跑一轮。
	drift, failed := false, false
	for _, f := range files {
		changed, err := syncFile(f, *check)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			failed = true
			continue
		}
		if changed {
			drift = true
			fmt.Printf("%s: §跳读 %s\n", f, ifStr(*check, "drift", "synced"))
		}
	}

	if failed {
		os.Exit(2)
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

	// 3. 覆盖度校验：每个 `## <id>` header 都必须在 §跳读 里有 entry。
	// 只刷新行号不足以保住导航——新增 header 忘了加 entry 时，§跳读 依然"无 drift"，
	// 该章节对按 §跳读 跳读的 AI 读者等于不存在（M08 的 3-quater/3-quinquies/§3-sexies
	// 就是这样漏了三节）。缺口在此报出，交由人工补 title（行号仍由脚本填）。
	if missing := missingTocEntries(lines[tocIdx], headers); len(missing) > 0 {
		return false, fmt.Errorf("§跳读 缺少章节条目: %s（补 `id:标题` 后再跑 make docs-sync 填行号）",
			strings.Join(missing, ", "))
	}

	orig := lines[tocIdx]
	newLine := rebuildToc(orig, headers, len(lines))
	if newLine == orig {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	lines[tocIdx] = newLine
	return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// missingTocEntries 返回有 header 但 §跳读 中无对应 entry 的 id 列表（按行号升序）。
func missingTocEntries(tocLine string, headers map[string]int) []string {
	present := map[string]bool{}
	body := tocLine[strings.Index(tocLine, tocPrefix)+len(tocPrefix):]
	body = strings.TrimSuffix(strings.TrimSpace(body), trailingCommentSuffix)
	for _, e := range strings.Split(body, " / ") {
		if m := entryLineRe.FindStringSubmatch(strings.TrimSpace(e)); m != nil {
			present[m[1]] = true
		}
	}

	var missing []string
	for id := range headers {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return headers[missing[i]] < headers[missing[j]] })
	return missing
}

// rebuildToc 重建一行 §跳读 文本。保留行首所有前缀 (如 `> `)。
// totalLines 供 rewriteEntry 判定"前导整数是否可能是旧行号"。
func rebuildToc(line string, headers map[string]int, totalLines int) string {
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
		entries[i] = rewriteEntry(strings.TrimSpace(e), headers, totalLines)
	}
	return head + " " + strings.Join(entries, " / ") + tail
}

// rewriteEntry 重写单个 entry。无匹配 header 时原样保留。
func rewriteEntry(entry string, headers map[string]int, totalLines int) string {
	m := entryLineRe.FindStringSubmatch(entry)
	if m == nil {
		return entry // 不符合 `id:rest` 格式 — 保留
	}
	id, rest := m[1], strings.TrimSpace(m[2])

	actualLine, ok := headers[id]
	if !ok {
		return entry // 子节锚或未匹配 — 保留
	}

	return fmt.Sprintf("%s:%d %s", id, actualLine, stripStaleLineNums(rest, totalLines))
}

// stripStaleLineNums 剥掉 title 前所有陈旧行号。
//
// 只剥"落在 [1, totalLines] 区间内的纯整数"，而不是无条件剥前导数字：
// 前者能证明该 token 曾是本文件的行号，后者会误伤真以数字开头的标题。
//
// 循环而非单次：headerRe 修复前（2026-08-10）本工具对大部分 header 无效，
// 历史上留下过 `15:408 428(SOFT)降级` 这类"新行号 + 旧行号 + 标题"的双号污染
// （全仓 7 处）。单次剥离只能清掉一层，旧号会永久沉淀进标题文本。
func stripStaleLineNums(rest string, totalLines int) string {
	title := rest
	for {
		mm := leadingNumRe.FindStringSubmatch(title)
		if mm == nil {
			return title
		}
		n, err := strconv.Atoi(mm[1])
		if err != nil || n < 1 || n > totalLines {
			return title // 不是本文件的行号 —— 是标题的一部分，停手
		}
		title = mm[2]
	}
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
