//go:build ignore

// review_merge 把各批次审核报告机械合并为全局报告、去重索引、lint-backlog 与类别收敛统计。
//
// 立此工具的原因（2026-08-12）：合并步骤在提示词里写得很清楚——「只做拼接与排序，
// 禁止改写条目内容」——但它每轮都由 LLM 现场写脚本执行。2026-08-11 那轮在
// local_playground/reports/ 下留下了 merge.py 与 merge_v2.py 两个临时版本，且合并
// 产物的条目正文与批次报告原文并不一致（批次报告用了自创的 `### [P2] 标题` 格式，
// 合并方只能重写成规范格式）——这已经越过了「只做拼接」的边界，且下一轮换个模型
// 会再发明第三种格式。
//
// 合并是纯机械操作，不该由 LLM 每轮重新发明。本工具固化它，并把两件原本靠模型
// 自觉填写、实测 100% 空转的事情改为机械计算：
//   - 类别收敛统计表（实测 10 行全为 "-"）：从条目的「违反规则」字段按 §类别映射梯
//     机械归类，「上一轮」列从上一份合并报告里读，趋势自动算；
//   - lint-backlog 的「问题类别」（实测 17/20 填「未分类」）：同一套映射，不再由模型填。
//
// 刻意不做：改写条目正文、合并「同一根因」的跨批次条目。前者违反合并纪律；后者需要
// 语义判断，机械做必然误合——本工具只把疑似同根因（同文件同行号）的条目并列标注，
// 判定留给人工或修复轮。
//
// 用法:
//
//	go run tools/review_merge.go              # 合并 local_playground/reports/
//	go run tools/review_merge.go -dry-run     # 只打印统计，不写文件
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const reportDir = "local_playground/reports"

var (
	entryRe     = regexp.MustCompile(`^###\s+\[([A-Z]{2}-[0-9A-Za-z.\-]+)\]\s*(.*)$`)
	fieldRe     = regexp.MustCompile(`^-\s+\*{0,2}([^:：*]+?)\*{0,2}\s*[:：]\s*(.*)$`)
	batchFileRe = regexp.MustCompile(`batch([0-9]+(?:\.[0-9]+)*)\.md$`)
	sevOrder    = map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3, "": 4}
	prioOrder   = map[string]int{"高": 0, "中": 1, "低": 2, "": 3}
)

// 类别映射梯：自上而下第一个命中者胜出。顺序即优先级，改动会影响跨轮趋势可比性，
// 改之前先想清楚——统计表的价值全在「同一口径连续多轮」。
var categoryLadder = []struct {
	name string
	keys []string
}{
	{"接线断裂（G-bis）", []string{"G-bis", "接线断裂", "无生产调用", "未接线"}},
	{"注释漂移（G）", []string{"注释漂移", "维度G-注释", "nolint"}},
	{"Taint 传播断点（D）", []string{"Taint", "污点", "taint"}},
	{"幂等重放与一致性（M）", []string{"幂等", "重放", "Outbox", "单调", "去重"}},
	{"生命周期与关停（L）", []string{"关停", "Shutdown", "优雅退出", "启动顺序", "迁移", "版本核对"}},
	{"并发与资源（C）", []string{"goroutine", "锁", "死锁", "竞态", "泄漏", "并发", "SafeGo"}},
	{"错误处理与边界（E）", []string{"吞", "errors.New", "fmt.Errorf", "apperr", "rows.Err", "静默", "panic"}},
	{"Schema/配置漂移（F）", []string{"Schema", "DDL", "ALTER TABLE", "threshold-examples", "配置项"}},
	{"docs↔code 漂移（A/K）", []string{"文档", "docs", "CLAUDE.md", "M0", "M1", "漂移", "失效引用"}},
	{"LLM/Agent 生产陷阱（H）", []string{"A-0", "A-1", "Prompt", "提示词", "重试", "TokenBurn", "RAG"}},
	{"HE 不变量违反（B）", []string{"HE-", "HE-Rule"}},
}

var allCategories = func() []string {
	out := make([]string, 0, len(categoryLadder)+1)
	for _, c := range categoryLadder {
		out = append(out, c.name)
	}
	return append(out, "其他")
}()

type entry struct {
	id, title, kind, batch string
	fields                 map[string]string
	raw                    string // 原文块，逐字保留
	src                    string
}

func (e entry) category() string {
	hay := e.fields["违反规则"] + " " + e.fields["类别"] + " " + e.title
	for _, c := range categoryLadder {
		for _, k := range c.keys {
			if strings.Contains(hay, k) {
				return c.name
			}
		}
	}
	return "其他"
}

func main() {
	dir := flag.String("dir", reportDir, "报告目录")
	dry := flag.Bool("dry-run", false, "只打印统计，不写文件")
	flag.Parse()

	files, _ := filepath.Glob(filepath.Join(*dir, "*.md"))
	sort.Strings(files)

	var findings, designs []entry
	for _, f := range files {
		base := filepath.Base(f)
		if !batchFileRe.MatchString(base) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, e := range parse(string(b), base) {
			switch e.kind {
			case "GR", "DR":
				findings = append(findings, e)
			case "GD", "DS":
				designs = append(designs, e)
			}
		}
	}

	findings, dupF := dedup(findings)
	designs, dupD := dedup(designs)
	if dupF+dupD > 0 {
		fmt.Printf("review-merge: 跳过 %d 条重复 ID（子分片报告与其批次聚合报告同时存在时的正常现象，保留分片版）\n", dupF+dupD)
	}

	if len(findings) == 0 && len(designs) == 0 {
		fmt.Println("review-merge: 未从批次报告解析到任何规范条目——条目须写成 `### [ID] 标题` + `- 字段: 值`；先跑 make review-check 定位格式问题")
		os.Exit(1)
	}

	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if sevOrder[a.fields["严重级"]] != sevOrder[b.fields["严重级"]] {
			return sevOrder[a.fields["严重级"]] < sevOrder[b.fields["严重级"]]
		}
		return a.id < b.id
	})
	sort.SliceStable(designs, func(i, j int) bool {
		a, b := designs[i], designs[j]
		if prioOrder[a.fields["优先级建议"]] != prioOrder[b.fields["优先级建议"]] {
			return prioOrder[a.fields["优先级建议"]] < prioOrder[b.fields["优先级建议"]]
		}
		return a.id < b.id
	})

	prev := readPrevCategoryCounts(filepath.Join(*dir, "gemini-review-findings.md"))
	cur := map[string]int{}
	for _, e := range findings {
		cur[e.category()]++
	}

	fmt.Printf("review-merge: 缺陷 %d 条、设计挑战 %d 条\n", len(findings), len(designs))
	for _, c := range allCategories {
		if cur[c] > 0 || prev[c] > 0 {
			fmt.Printf("  %-24s 本轮 %2d  上一轮 %2d  %s\n", c, cur[c], prev[c], trend(cur[c], prev[c]))
		}
	}
	if *dry {
		return
	}

	write(filepath.Join(*dir, "gemini-review-findings.md"), buildFindings(findings, cur, prev))
	write(filepath.Join(*dir, "gemini-review-design.md"), buildDesign(designs))
	write(filepath.Join(*dir, "known-findings-index.md"), buildIndex(filepath.Join(*dir, "known-findings-index.md"), findings))
	write(filepath.Join(*dir, "lint-backlog.md"), buildBacklog(filepath.Join(*dir, "lint-backlog.md"), findings))
	fmt.Println("review-merge: 已写入 gemini-review-findings.md / gemini-review-design.md / known-findings-index.md / lint-backlog.md")
}

// ---------- 解析 ----------

func parse(text, src string) []entry {
	lines := strings.Split(text, "\n")
	var out []entry
	var cur *entry
	var buf []string
	inCode := false

	flush := func() {
		if cur != nil {
			cur.raw = strings.TrimRight(strings.Join(buf, "\n"), "\n")
			out = append(out, *cur)
			cur = nil
			buf = nil
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
		}
		if m := entryRe.FindStringSubmatch(line); m != nil && !inCode {
			flush()
			id := m[1]
			batch := ""
			if p := strings.Split(id, "-"); len(p) >= 3 {
				batch = p[1]
			}
			cur = &entry{id: id, title: strings.TrimSpace(m[2]), kind: id[:2], batch: batch,
				fields: map[string]string{}, src: src}
			buf = []string{line}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(line, "## ") && !inCode {
			flush()
			continue
		}
		buf = append(buf, line)
		if m := fieldRe.FindStringSubmatch(line); m != nil && !inCode {
			k := strings.TrimSpace(m[1])
			if _, dup := cur.fields[k]; !dup {
				cur.fields[k] = strings.TrimSpace(m[2])
			}
		}
	}
	flush()
	return out
}

// ---------- 产出 ----------

func buildFindings(es []entry, cur, prev map[string]int) string {
	var b strings.Builder
	b.WriteString("<!-- 由 make review-merge 生成，勿手改；条目正文逐字取自批次报告 -->\n")
	b.WriteString("# 审核发现汇总（GR/DR）\n\n## 全局汇总表\n")
	b.WriteString("| ID | 严重级 | 模块 | 一句话标题 | 置信度 | 可机械化 | 类别 | 来源 |\n|---|---|---|---|---|---|---|---|\n")
	for _, e := range es {
		mod := first(e.fields["模块"], e.fields["对象"])
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			e.id, dash(e.fields["严重级"]), dash(mod), e.title,
			dash(e.fields["置信度"]), dash(e.fields["可机械化"]), e.category(), e.src)
	}

	b.WriteString("\n## 类别收敛统计\n")
	b.WriteString("> 机械统计（映射梯见 tools/review_merge.go categoryLadder）。判读规则：某类别连续两轮不降 → 该类门控缺失，优先落地对应 lint 规则，而不是下一轮更用力地扫它。\n\n")
	b.WriteString("| 缺陷类别 | 本轮 | 上一轮 | 趋势 |\n|---|---|---|---|\n")
	for _, c := range allCategories {
		p := "—"
		if len(prev) > 0 {
			p = fmt.Sprintf("%d", prev[c])
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s |\n", c, cur[c], p, trend(cur[c], prev[c]))
	}

	b.WriteString("\n## 疑似同根因（同文件同行号，人工判定是否合并）\n")
	byLoc := map[string][]string{}
	for _, e := range es {
		loc := first(e.fields["位置"], e.fields["代码位置"])
		if loc == "" || loc == "—" {
			continue
		}
		byLoc[loc] = append(byLoc[loc], e.id)
	}
	dup := 0
	for _, loc := range sortedKeys(byLoc) {
		if len(byLoc[loc]) > 1 {
			fmt.Fprintf(&b, "- %s ← %s\n", loc, strings.Join(byLoc[loc], ", "))
			dup++
		}
	}
	if dup == 0 {
		b.WriteString("- 无\n")
	}

	b.WriteString("\n## 详细发现清单\n\n")
	for _, e := range es {
		b.WriteString(e.raw + "\n\n")
	}
	return b.String()
}

func buildDesign(es []entry) string {
	var b strings.Builder
	b.WriteString("<!-- 由 make review-merge 生成，勿手改 -->\n# 设计挑战汇总（GD/DS）\n\n")
	b.WriteString("| ID | 类别 | 涉及模块 | 一句话标题 | 优先级 | 来源 |\n|---|---|---|---|---|---|\n")
	for _, e := range es {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			e.id, dash(e.fields["类别"]), dash(first(e.fields["涉及模块"], e.fields["现状"])),
			e.title, dash(e.fields["优先级建议"]), e.src)
	}
	b.WriteString("\n## 详细条目\n\n")
	for _, e := range es {
		b.WriteString(e.raw + "\n\n")
	}
	return b.String()
}

// buildIndex 保留既有条目的 fixed/rejected 状态——索引是跨轮去重的唯一载体，
// 覆盖写会把修复轮的成果抹掉（这类"重建即丢状态"的事故本仓库已发生过多次）。
func buildIndex(path string, es []entry) string {
	old := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			c := splitRow(l)
			if len(c) >= 5 && strings.Contains(c[0], "-") {
				old[c[0]] = c[4]
			}
		}
	}
	var b strings.Builder
	b.WriteString("<!-- 由 make review-merge 生成；状态列人工/修复轮维护，合并时保留 -->\n")
	b.WriteString("| ID | 模块 | 位置 | 一句话标题 | 状态(open/fixed/rejected) |\n|---|---|---|---|---|\n")
	for _, e := range es {
		st := old[e.id]
		if st == "" {
			st = "open"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			e.id, dash(first(e.fields["模块"], e.fields["对象"])),
			dash(first(e.fields["位置"], e.fields["文档位置"])), e.title, st)
	}
	return b.String()
}

func buildBacklog(path string, es []entry) string {
	old := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			c := splitRow(l)
			if len(c) >= 4 && strings.Contains(c[0], "-") {
				old[c[0]] = c[3]
			}
		}
	}
	var b strings.Builder
	b.WriteString("<!-- 由 make review-merge 生成；状态列由修复轮维护（见 prompt/fix.md）-->\n")
	b.WriteString("# 可机械化待办\n\n")
	b.WriteString("> 修复轮纪律：本表 pending 清零（landed 或带理由 rejected）之后，才允许开始修 GR 条目。\n")
	b.WriteString("> 规则先行的理由：已落地的规则能自动验证 GR 修复的完备性；反序则规则永远排在「更紧急的修复」之后，无限延期。\n\n")
	b.WriteString("| 来源ID | 问题类别 | 建议规则 | 状态(pending/landed/rejected) |\n|---|---|---|---|\n")
	n := 0
	for _, e := range es {
		if !strings.HasPrefix(e.fields["可机械化"], "是") {
			continue
		}
		rule := extractRule(e.fields["可机械化"])
		if rule == "" {
			rule = "（未填写建议规则——review-check C10 会拦）"
		}
		st := old[e.id]
		if st == "" {
			st = "pending"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", e.id, e.category(), rule, st)
		n++
	}
	if n == 0 {
		b.WriteString("\n> 本轮 0 条可机械化。合并报告头部必须写明「本轮 0 条可机械化，已逐条复核」，否则 review-check C10 报红——\n")
		b.WriteString("> 该字段没人认真填正是 2026-08-09 收敛机制整轮空转的根因。\n")
	}
	return b.String()
}

// ---------- 辅助 ----------

func readPrevCategoryCounts(path string) map[string]int {
	out := map[string]int{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	in := false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.Contains(l, "类别收敛统计") {
			in = true
			continue
		}
		if in && strings.HasPrefix(l, "## ") {
			break
		}
		if !in || !strings.HasPrefix(l, "|") {
			continue
		}
		c := splitRow(l)
		if len(c) < 2 || c[0] == "缺陷类别" || strings.HasPrefix(c[0], "-") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(c[1], "%d", &n); err == nil {
			out[c[0]] = n
		}
	}
	return out
}

// dedup 按条目 ID 去重。同一 ID 出现多次通常是「子分片报告 batch9.2.2.md 与其批次
// 聚合报告 batch9.md 同时存在」——优先保留文件名批次段与 ID 批次段更接近的那份
// （分片版信息更完整），避免同一发现在全局表里出现两行。
func dedup(es []entry) ([]entry, int) {
	best := map[string]entry{}
	order := []string{}
	dropped := 0
	for _, e := range es {
		old, ok := best[e.id]
		if !ok {
			best[e.id] = e
			order = append(order, e.id)
			continue
		}
		dropped++
		if specificity(e) > specificity(old) {
			best[e.id] = e
		}
	}
	out := make([]entry, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out, dropped
}

// specificity：文件名里的批次段越长（分片越细）越具体。
func specificity(e entry) int {
	m := batchFileRe.FindStringSubmatch(e.src)
	if m == nil {
		return 0
	}
	return len(m[1])
}

// extractRule 从「可机械化: 是（建议规则: xxx）」中取出规则正文。
func extractRule(v string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "是"))
	s = strings.Trim(s, "（()） ")
	for _, p := range []string{"建议规则:", "建议规则：", "规则:", "规则："} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	return strings.TrimSpace(strings.Trim(s, "（()） "))
}

func trend(cur, prev int) string {
	switch {
	case prev == 0 && cur == 0:
		return "—"
	case cur < prev:
		return "↓"
	case cur > prev:
		return "↑"
	default:
		return "持平 ⚠ 连续不降=该类门控缺失"
	}
}

func splitRow(l string) []string {
	l = strings.TrimSpace(l)
	if !strings.HasPrefix(l, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(l, "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func first(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func write(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "review-merge: 写 %s 失败: %v\n", path, err)
		os.Exit(2)
	}
}
