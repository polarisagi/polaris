//go:build ignore

// review_check 校验 local_playground/reports/ 下审核报告的机械可判属性。
//
// 立此门控的原因（2026-08-12 复盘 2026-08-11 首轮 14 批实测产物）：
// 审核提示词 v3 新增的四项强制机制——「可机械化字段全否复核」「G-bis 接线专项」
// 「8 条端到端链路走查」「类别收敛统计表」——在首轮**全部空转**：
//   - 类别收敛统计表 10 行全为 "-"；
//   - 8 条链路零条给出「已走通/断链」结论；
//   - lint-backlog 20 条中 17 条问题类别填「未分类」，跨轮聚合不可能；
//   - 置信度 37/37 全填「高」，而该来源「未接线」类结论历史误报率为 5/7；
//   - 批次 11 在 review-progress.md 标 done 且声明区间 GR-11-001~004，
//     但报告文件不存在、合并报告与索引各 0 条——4 条发现直接蒸发，
//     编排 Agent 的「验收」环节没拦住（它按写死的文件名找，而子 Agent 分片改了名）。
//
// 这与 v2 之前 lint-backlog 空转是同一种失败，只是换了字段名。规律：提示词里
// **有机械产物的部分都执行了，靠模型自觉填写的部分 100% 空转**。继续往提示词里
// 加条款是在与其余 360 行争夺注意力；门控红灯不争注意力，它只是拒绝提交。
// 本工具即该转化，思路同 ADR-0062 deadcode / ADR-0081 docs-refs / adr_index_check。
//
// 刻意不做的检查：条目「问题」「挑战」「建议方案」等正文的质量判断。那是判断密集
// 内容，机械校验只能查字段存在性与枚举合法性，硬做语义判定必然大量误报——误报
// 淹没真实缺陷比漏报更致命（沿用 ADR-0081 决策一同款判断）。质量留给复扫轮与人工。
//
// 用法:
//
//	go run tools/review_check.go                 # 全量校验（棘轮：baseline 内的违规降为警告）
//	go run tools/review_check.go -strict         # 忽略 baseline，全部按错误处理
//	go run tools/review_check.go -update-baseline # 把当前违规全部写入 baseline
//	go run tools/review_check.go -dir <报告目录>
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultReportDir = "local_playground/reports"
	baselinePath     = "scripts/review-check-baseline.txt"
	scopePath        = "scripts/review-batch-scope.txt"
)

// ---------- 枚举表（与提示词 §7 输出格式逐字对应，改一处必须改另一处）----------

var (
	enumSeverity   = set("P0", "P1", "P2", "P3")
	enumConfidence = set("高", "中", "低")
	enumBool       = set("是", "否")
	// GD 类别（代码轨道设计挑战）
	enumGDCategory = set("功能缺失", "调用链问题", "模块拆合", "过度设计",
		"错误路径缺失", "业界对标差距", "领先设计(保留)")
	// DS 类别（文档轨道结构建议）
	enumDSCategory = set("文档拆分", "文档合并", "新建文档", "新立 ADR", "导航重构")
	// DR 归因（文档轨道冲突三分法，§5-C）
	enumAttribution = set("文档超前", "文档落后", "代码违背设计", "待人工")
	// AD 处置动作（ADR 生命周期六选一，§6.1）
	enumADAction  = set("KEEP", "AMEND", "SUPERSEDE", "MERGE", "ARCHIVE", "DELETE")
	enumADFulfill = set("已兑现", "部分兑现", "未兑现", "已回退")
	// lint-backlog 问题类别 = 类别收敛统计表的行名，两处必须同源，否则跨轮聚合失效
	enumFindingCategory = []string{
		"接线断裂（G-bis）", "注释漂移（G）", "Taint 传播断点（D）", "并发与资源（C）",
		"错误处理与边界（E）", "docs↔code 漂移（A/K）", "Schema/配置漂移（F）",
		"LLM/Agent 生产陷阱（H）", "HE 不变量违反（B）", "生命周期与关停（L）",
		"幂等重放与一致性（M）", "其他",
	}
	enumBacklogStatus  = set("pending", "landed", "rejected")
	enumProgressStatus = set("pending", "running", "done", "failed")
)

// 每类条目的必填字段。缺字段即红——「填不出来」本身就是该条不成立的信号。
var requiredFields = map[string][]string{
	"GR": {"严重级", "模块", "位置", "违反规则", "置信度", "可机械化", "反证", "问题", "证据"},
	"GD": {"类别", "涉及模块", "现状", "挑战", "ADR 核对", "业界依据", "建议方案", "代价/收益", "优先级建议"},
	"DR": {"严重级", "对象", "文档位置", "代码位置", "归因", "归因证据", "违反规则", "置信度", "需代码侧修复", "问题", "证据", "建议改法"},
	"AD": {"文件", "README 状态", "实测兑现", "兑现证据", "台账失真", "处置动作", "处置依据", "引用方核查"},
	"DS": {"类别", "现状", "问题", "建议方案", "代价", "优先级建议"},
}

// ---------- 正则 ----------

var (
	// 条目标题：### [GR-9.2.1-001] xxx / ### [AD-0033] xxx
	entryRe = regexp.MustCompile(`^###\s+\[([A-Z]{2}-[0-9A-Za-z.\-]+)\]\s*(.*)$`)
	// 字段行：- 严重级: P0   /   - **位置**: xxx（兼容加粗，但会单独报格式警告）
	fieldRe = regexp.MustCompile(`^-\s+\*{0,2}([^:：*]+?)\*{0,2}\s*[:：]\s*(.*)$`)
	// ID 规范：GR-<批次>-<3位序号>，批次段允许 N.M.K 分片
	idRe = regexp.MustCompile(`^(GR|GD|DR|DS)-([0-9]+(?:\.[0-9]+)*)-([0-9]{3})$`)
	adRe = regexp.MustCompile(`^AD-([0-9]{4})$`)
	// 批次报告文件名：...-batch<N>[.M][.K].md
	batchFileRe = regexp.MustCompile(`batch([0-9]+(?:\.[0-9]+)*)\.md$`)
	// 位置引用：path/to/file.go:123 或 :123-456
	locRe = regexp.MustCompile(`([A-Za-z0-9_./\-]+\.(?:go|rs|sql|md|toml|yaml|yml|sh)):([0-9]+)(?:-([0-9]+))?`)
	// 进度表行：| 1 | done | GR-1-001~GR-1-014 | 2026-xx-xx |
	progressRowRe = regexp.MustCompile(`^\|\s*([0-9]+(?:\.[0-9]+)*)\s*\|\s*([^|]+?)\s*\|\s*([^|]*?)\s*\|`)
	// 链路走查结论：链路3: 已走通  /  链路 3：断链(GD-13-002)
	linkRe = regexp.MustCompile(`链路\s*([1-8])\s*[:：]\s*(已走通|断链)`)
	// 覆盖凭证节内的路径/文件名 token
	pathTokenRe = regexp.MustCompile("[`\"']?[A-Za-z0-9_./\\-]+\\.(?:go|rs|sql|toml|md)[`\"']?")
	// 独立成行的粗体标签，作为小节锚点的备选写法：`- **已审文件清单**:`
	boldLabelRe = regexp.MustCompile(`^-?\s*\*\*[^*]+\*\*\s*[:：]?\s*$`)
	// 疑似条目标题但缺方括号：`### GR-7.1-001：xxx`。不拦住的话整批发现会在合并时静默消失。
	malformedEntryRe = regexp.MustCompile(`^###+\s+\*{0,2}((?:GR|GD|DR|DS|AD)-[0-9][0-9A-Za-z.\-]*)`)
)

// ---------- 问题记录 ----------

type problem struct {
	check string // C1..C11
	file  string
	key   string // 条目 ID 或定位键，构成 baseline 签名的一部分
	msg   string
}

func (p problem) sig() string { return p.file + "|" + p.check + "|" + p.key }

type checker struct {
	root      string // 仓库根
	reportDir string
	problems  []problem
	fileLines map[string]int // 源文件行数缓存
}

func (c *checker) add(check, file, key, format string, a ...any) {
	c.problems = append(c.problems, problem{check, file, key, fmt.Sprintf(format, a...)})
}

func main() {
	var (
		dir      = flag.String("dir", defaultReportDir, "报告目录")
		strict   = flag.Bool("strict", false, "忽略 baseline，全部按错误处理")
		update   = flag.Bool("update-baseline", false, "把当前违规写入 baseline")
		quietOK  = flag.Bool("q", false, "只输出错误")
		rootFlag = flag.String("root", ".", "仓库根目录")
	)
	flag.Parse()

	rd := *dir
	if !filepath.IsAbs(rd) {
		rd = filepath.Join(*rootFlag, rd)
	}
	c := &checker{root: *rootFlag, reportDir: rd, fileLines: map[string]int{}}

	if _, err := os.Stat(c.reportDir); os.IsNotExist(err) {
		fmt.Printf("review-check: 报告目录 %s 不存在，跳过（尚未跑过审核轮）\n", c.reportDir)
		return
	}

	c.run()

	sort.Slice(c.problems, func(i, j int) bool { return c.problems[i].sig() < c.problems[j].sig() })

	if *update {
		if err := writeBaseline(filepath.Join(*rootFlag, baselinePath), c.problems); err != nil {
			fail("写 baseline 失败: %v", err)
		}
		fmt.Printf("review-check: 已写入 baseline %d 条 → %s\n", len(c.problems), baselinePath)
		return
	}

	base := map[string]bool{}
	if !*strict {
		base = readBaseline(filepath.Join(*rootFlag, baselinePath))
	}

	var errs, warns []problem
	for _, p := range c.problems {
		if base[p.sig()] {
			warns = append(warns, p)
		} else {
			errs = append(errs, p)
		}
	}

	if len(warns) > 0 && !*quietOK {
		fmt.Printf("review-check: %d 条存量违规（baseline 内，不阻断）\n", len(warns))
	}
	if len(errs) == 0 {
		fmt.Printf("review-check ok（存量 %d，新增 0）\n", len(warns))
		return
	}
	fmt.Printf("\nreview-check FAIL: %d 条新增违规\n\n", len(errs))
	for _, p := range errs {
		fmt.Printf("  [%s] %s :: %s\n        %s\n", p.check, p.file, p.key, p.msg)
	}
	fmt.Printf("\n修复后重跑；确属刻意保留的存量可用 -update-baseline 收录（须逐条带理由，禁止批量填充）\n")
	os.Exit(1)
}

// ---------- 主流程 ----------

func (c *checker) run() {
	files, err := filepath.Glob(filepath.Join(c.reportDir, "*.md"))
	if err != nil {
		fail("扫描报告目录失败: %v", err)
	}
	sub, _ := filepath.Glob(filepath.Join(c.reportDir, "arch-audit", "*.md"))
	files = append(files, sub...)
	sort.Strings(files)

	batchEntries := map[string][]string{} // 批次号 → 该批次报告内的条目 ID
	batchFiles := map[string][]string{}   // 批次号 → 报告文件

	for _, f := range files {
		rel := c.rel(f)
		body, err := os.ReadFile(f)
		if err != nil {
			c.add("C0", rel, "-", "读取失败: %v", err)
			continue
		}
		text := string(body)

		switch {
		case strings.HasSuffix(rel, "lint-backlog.md"):
			c.checkBacklog(rel, text)
			continue
		case strings.HasSuffix(rel, "review-progress.md"), strings.HasSuffix(rel, "audit-progress.md"):
			continue // 进度表在 checkProgress 里统一处理（需要 batchEntries）
		case strings.HasSuffix(rel, "known-findings-index.md"), strings.HasSuffix(rel, "known-doc-findings-index.md"):
			continue
		}

		entries := parseEntries(text)
		merged := !batchFileRe.MatchString(rel)

		if m := batchFileRe.FindStringSubmatch(rel); m != nil {
			b := m[1]
			// 同时按分片号与顶层批次号登记：进度表写的是「7」，报告文件可能叫
			// batch7.1/batch7.2。不做这层归并，分片过的批次在 C4 眼里就是「标 done
			// 却找不到报告」——那正是批次 11 蒸发时的形态，换个马甲还会再来一次。
			keys := []string{b}
			if i := strings.Index(b, "."); i > 0 {
				keys = append(keys, b[:i])
			}
			for _, k := range keys {
				batchFiles[k] = append(batchFiles[k], rel)
				for _, e := range entries {
					batchEntries[k] = append(batchEntries[k], e.id)
				}
			}
			c.checkBatchFile(rel, b, text, entries)
		}

		for _, e := range entries {
			c.checkEntry(rel, e)
		}
		if merged {
			c.checkMerged(rel, text, entries)
		}
	}

	c.checkProgress(batchEntries, batchFiles)
}

// ---------- 条目解析 ----------

type entry struct {
	id     string
	title  string
	kind   string // GR/GD/DR/DS/AD
	batch  string
	fields map[string]string
	line   int
}

func parseEntries(text string) []entry {
	var out []entry
	var cur *entry
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	ln := 0
	inCode := false
	for sc.Scan() {
		ln++
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
		}
		if m := entryRe.FindStringSubmatch(line); m != nil && !inCode {
			if cur != nil {
				out = append(out, *cur)
			}
			id := m[1]
			kind := id[:2]
			batch := ""
			if mm := idRe.FindStringSubmatch(id); mm != nil {
				batch = mm[2]
			}
			cur = &entry{id: id, title: strings.TrimSpace(m[2]), kind: kind, batch: batch,
				fields: map[string]string{}, line: ln}
			continue
		}
		if cur == nil || inCode {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			out = append(out, *cur)
			cur = nil
			continue
		}
		if m := fieldRe.FindStringSubmatch(line); m != nil {
			k := strings.TrimSpace(m[1])
			if _, dup := cur.fields[k]; !dup {
				cur.fields[k] = strings.TrimSpace(m[2])
			}
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// ---------- C1/C2/C3：条目级校验 ----------

func (c *checker) checkEntry(file string, e entry) {
	// C1 ID 规范
	if e.kind == "AD" {
		if !adRe.MatchString(e.id) {
			c.add("C1", file, e.id, "AD 条目 ID 应为 AD-00XX（4 位 ADR 编号）")
		}
	} else if !idRe.MatchString(e.id) {
		c.add("C1", file, e.id, "ID 不符合 <类型>-<批次>-<3位序号>；实测曾出现 GR-03-01 与 GR-9.2.1-001 两种写法并存，破坏跨轮去重")
	}
	// C1b ID 批次段与文件名批次一致
	if m := batchFileRe.FindStringSubmatch(file); m != nil && e.batch != "" && e.kind != "AD" {
		if !strings.HasPrefix(m[1], e.batch) && !strings.HasPrefix(e.batch, m[1]) {
			c.add("C1", file, e.id, "ID 批次段 %s 与文件名批次 %s 不一致", e.batch, m[1])
		}
	}

	req, ok := requiredFields[e.kind]
	if !ok {
		c.add("C1", file, e.id, "未知条目类型 %s（合法：GR/GD/DR/DS/AD）", e.kind)
		return
	}
	for _, f := range req {
		if strings.TrimSpace(e.fields[f]) == "" {
			c.add("C3", file, e.id, "缺必填字段「%s」", f)
		}
	}

	// C3 枚举合法性
	c.enumField(file, e, "严重级", enumSeverity)
	c.enumField(file, e, "置信度", enumConfidence)
	c.enumField(file, e, "可机械化", enumBool, "（", "(")
	c.enumField(file, e, "需代码侧修复", enumBool)
	c.enumField(file, e, "台账失真", enumBool, "（")
	c.enumField(file, e, "归因", enumAttribution)
	c.enumField(file, e, "处置动作", enumADAction)
	c.enumField(file, e, "实测兑现", enumADFulfill)
	c.enumField(file, e, "优先级建议", set("高", "中", "低"))
	switch e.kind {
	case "GD":
		c.enumField(file, e, "类别", enumGDCategory)
	case "DS":
		c.enumField(file, e, "类别", enumDSCategory)
	}

	// C2 位置行号真实性——直接抓幻觉，现有自检清单靠打勾，实测无效
	for _, f := range []string{"位置", "文档位置", "代码位置", "兑现证据", "文件"} {
		v := e.fields[f]
		if v == "" || v == "—" {
			continue
		}
		for _, m := range locRe.FindAllStringSubmatch(v, -1) {
			c.checkLoc(file, e.id, f, m)
		}
	}

	// C11 GR 的「反证」字段必须实质填写：接线断裂类强制列出已核对的入口
	if e.kind == "GR" {
		rebut := e.fields["反证"]
		rule := e.fields["违反规则"] + " " + e.title
		if strings.Contains(rule, "G-bis") || strings.Contains(rule, "接线断裂") || strings.Contains(rule, "无生产调用") {
			if !strings.Contains(rebut, "boot_") && !strings.Contains(rebut, "bootstrap") {
				c.add("C11", file, e.id, "接线断裂类必须在「反证」中写明已核对 cmd/polaris/boot_*.go 与 internal/bootstrap/（历史误报 5/7 全部因跳过此步）")
			}
		}
		if sev := e.fields["严重级"]; (sev == "P0" || sev == "P1") && len([]rune(rebut)) < 10 {
			c.add("C11", file, e.id, "P0/P1 的「反证」不得少于 10 字：须给出反证过程或可达路径")
		}
	}
}

func (c *checker) enumField(file string, e entry, name string, allowed map[string]bool, trimAt ...string) {
	v, ok := e.fields[name]
	if !ok || v == "" {
		return
	}
	for _, t := range trimAt {
		if i := strings.Index(v, t); i > 0 {
			v = strings.TrimSpace(v[:i])
		}
	}
	v = strings.TrimSpace(strings.Trim(v, "*`"))
	if !allowed[v] {
		c.add("C3", file, e.id, "字段「%s」取值 %q 不在枚举内 [%s]", name, v, joinSet(allowed))
	}
}

func (c *checker) checkLoc(file, id, field string, m []string) {
	p := m[1]
	if !strings.Contains(p, "/") {
		return // 裸文件名，无法定位，交由人工
	}
	abs := filepath.Join(c.root, p)
	n, ok := c.fileLines[p]
	if !ok {
		b, err := os.ReadFile(abs)
		if err != nil {
			c.add("C2", file, id, "「%s」引用的文件不存在: %s", field, p)
			c.fileLines[p] = -1
			return
		}
		n = strings.Count(string(b), "\n") + 1
		c.fileLines[p] = n
	}
	if n < 0 {
		return
	}
	for _, s := range []string{m[2], m[3]} {
		if s == "" {
			continue
		}
		v, err := strconv.Atoi(s)
		if err == nil && v > n {
			c.add("C2", file, id, "「%s」行号越界: %s:%d，该文件仅 %d 行", field, p, v, n)
		}
	}
}

// ---------- C5/C6/C8/C9：批次报告级校验 ----------

func (c *checker) checkBatchFile(file, batch, text string, entries []entry) {
	// C5 零发现是异常状态，不是合格结论。批次 7（11026 行）曾以 0 条 done 收场且无文件。
	if len(entries) == 0 && !strings.Contains(text, "零发现举证") {
		c.add("C5", file, "batch"+batch,
			"本批次 0 条发现但无「零发现举证」节：须列出已审文件清单、各侧重维度实际执行的 grep 命令与命中数、≥3 例判定为非问题的抽样理由")
	}
	// C1b 疑似条目标题但缺方括号——整批发现会在合并时静默消失，是批次 11 那类损失的新马甲
	for i, line := range strings.Split(text, "\n") {
		if m := malformedEntryRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			c.add("C1", file, m[1], "第 %d 行标题缺方括号：条目须写成 `### [%s] 标题`。当前写法解析不出条目，make review-merge 会把整批发现丢掉", i+1, m[1])
		}
	}

	// C6 覆盖凭证
	if !hasSection(text, "已审文件") {
		c.add("C6", file, "batch"+batch, "缺「已审文件清单」节——漏审不可见是首轮最大的隐性损失")
	}
	if !hasSection(text, "未覆盖") {
		c.add("C6", file, "batch"+batch, "缺「明确未覆盖的范围」节（无未覆盖也要显式写「无」）")
	}
	c.checkScope(file, batch, text)

	// C8 批次 13 的 8 条端到端链路：v3 已写「不允许留空」，实测 0 条落实
	if strings.HasPrefix(batch, "13") {
		got := map[string]bool{}
		for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
			got[m[1]] = true
		}
		var miss []string
		for i := 1; i <= 8; i++ {
			if !got[strconv.Itoa(i)] {
				miss = append(miss, strconv.Itoa(i))
			}
		}
		if len(miss) > 0 {
			c.add("C8", file, "batch13", "缺链路走查结论: 链路 %s（每条须写 `链路N: 已走通` 或 `链路N: 断链`）", strings.Join(miss, ","))
		}
	}

	// C9 置信度分布：全部同值时必须给出声明，否则该字段等于没填
	vals := map[string]int{}
	for _, e := range entries {
		if v := e.fields["置信度"]; v != "" {
			vals[v]++
		}
	}
	if len(vals) == 1 && len(entries) >= 3 && !strings.Contains(text, "置信度分布声明") {
		for v := range vals {
			c.add("C9", file, "batch"+batch,
				"全部 %d 条置信度均为「%s」，须在报告头写一行「置信度分布声明: <理由>」（该来源历史误报率 5/7，全高置信等于把判定成本推给修复方）", len(entries), v)
		}
	}
}

// C6-scope 批次范围覆盖率：scripts/review-batch-scope.txt 把批次表变成机器可读，
// 有它才谈得上「漏审可见」——首轮 11026 行的批次 7 以 0 条 done 收场，没有任何
// 机制能指出它究竟审没审。判定：本批次范围内每个非测试源文件，必须出现在报告的
// 「已审文件清单」或「明确未覆盖」两节之一；两节都没有 = 该文件去向不明。
// scope 文件缺失时降级为只查节存在性（上面已做）。
func (c *checker) checkScope(file, batch, text string) {
	scope := readScope(filepath.Join(c.root, scopePath))
	dirs, ok := scope[batch]
	if !ok || len(dirs) == 0 {
		return
	}
	declared := sectionPaths(text, "已审文件")
	uncovered := sectionPaths(text, "未覆盖")

	var missing []string
	for _, d := range dirs {
		for _, f := range c.scanSources(d) {
			base := filepath.Base(f)
			if declared[f] || declared[base] || uncovered[f] || uncovered[base] {
				continue
			}
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		show := missing
		if len(show) > 5 {
			show = show[:5]
		}
		c.add("C6", file, "batch"+batch,
			"本批次范围内 %d 个源文件既不在「已审文件清单」也不在「未覆盖」节，去向不明（前 5 例: %s）",
			len(missing), strings.Join(show, " "))
	}
}

// scanSources 列出目录下的非测试源文件（相对仓库根）。
func (c *checker) scanSources(dir string) []string {
	var out []string
	root := filepath.Join(c.root, dir)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "node_modules", "pb", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".rs") {
			out = append(out, c.rel(p))
		}
		return nil
	})
	return out
}

// sectionPaths 取出某个小节内出现的全部路径/文件名 token。
//
// 小节锚点接受两种写法：`## 已审文件清单` 标题，或 `- **已审文件清单**:` 粗体标签。
// 后者是容忍而非鼓励——实测 batch7.1 用粗体标签列全了 39 个文件，若只认标题就会
// 报「39 个文件去向不明」，那是纯误报：覆盖凭证在，只是排版不同。排版本身另由
// hasSection 单独提示，两个轴分开判，不互相污染。
func sectionPaths(text, keyword string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	in := false
	for sc.Scan() {
		l := sc.Text()
		t := strings.TrimSpace(l)
		if isSectionAnchor(t) {
			in = strings.Contains(t, keyword)
			continue
		}
		if !in {
			continue
		}
		for _, m := range pathTokenRe.FindAllString(l, -1) {
			out[strings.Trim(m, "`\"'")] = true
		}
	}
	return out
}

// isSectionAnchor 判断一行是否构成小节边界：markdown 标题，或独立成行的粗体标签。
func isSectionAnchor(t string) bool {
	if strings.HasPrefix(t, "#") {
		return true
	}
	return boldLabelRe.MatchString(t)
}

// hasSection 判断报告里是否存在某个小节（标题或粗体标签均可）。
func hasSection(text, keyword string) bool {
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if isSectionAnchor(t) && strings.Contains(t, keyword) {
			return true
		}
	}
	return false
}

// ---------- C7/C10：合并报告与 backlog ----------

func (c *checker) checkMerged(file, text string, entries []entry) {
	if !strings.Contains(text, "类别收敛统计") {
		if strings.Contains(file, "findings.md") && len(entries) > 0 {
			c.add("C7", file, "-", "合并报告缺「类别收敛统计」表")
		}
		return
	}
	// C7 统计表不得留空占位——实测 10 行全为 "-"
	sc := bufio.NewScanner(strings.NewReader(text))
	in := false
	empty := 0
	total := 0
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
		cols := splitRow(l)
		if len(cols) < 2 || cols[0] == "缺陷类别" || strings.HasPrefix(cols[0], "-") {
			continue
		}
		total++
		if cols[1] == "" || cols[1] == "-" || cols[1] == "—" {
			empty++
		}
	}
	if total > 0 && empty > 0 {
		c.add("C7", file, "-", "类别收敛统计表有 %d/%d 行「本轮」列为空占位——统计表空转是 v3 的实测失效点", empty, total)
	}
}

func (c *checker) checkBacklog(file, text string) {
	rows := 0
	pending := 0
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cols := splitRow(l)
		if len(cols) < 4 || cols[0] == "来源ID" || strings.HasPrefix(cols[0], "-") {
			continue
		}
		rows++
		id, cat, rule, st := cols[0], cols[1], cols[2], cols[3]
		if !idRe.MatchString(id) {
			c.add("C1", file, id, "来源 ID 不符合规范")
		}
		if !containsStr(enumFindingCategory, cat) {
			c.add("C10", file, id, "问题类别 %q 不在枚举内——实测 17/20 条填「未分类」，导致跨轮类别收敛统计无法机械聚合；合法值见 tools/review_check.go enumFindingCategory", cat)
		}
		if len([]rune(rule)) < 8 {
			c.add("C10", file, id, "「建议规则」过短，须含 grep 模式或 AST 断言")
		}
		if !enumBacklogStatus[st] {
			c.add("C10", file, id, "状态 %q 不在 {pending,landed,rejected}", st)
		}
		if st == "pending" {
			pending++
		}
	}
	if rows == 0 {
		c.add("C10", file, "-", "lint-backlog 无条目：若本轮 GR 全部「可机械化: 否」，须在合并报告头写明「本轮 0 条可机械化，已逐条复核」")
	}
}

// ---------- C4：进度表与产物一致性 ----------

func (c *checker) checkProgress(batchEntries, batchFiles map[string][]string) {
	for _, name := range []string{"review-progress.md", "audit-progress.md", "arch-audit/audit-progress.md"} {
		p := filepath.Join(c.reportDir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		rel := c.rel(p)
		for _, line := range strings.Split(string(b), "\n") {
			m := progressRowRe.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			batch, status, span := m[1], strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
			if !enumProgressStatus[status] {
				c.add("C4", rel, "batch"+batch, "状态 %q 不在 {pending,running,done,failed}——实测曾出现 completed 与 done 并存，任何脚本判定都会漏", status)
				continue
			}
			if status != "done" {
				// 状态非 done 但产物已存在 → 状态与产物矛盾（实测批次 13）
				if _, ok := batchFiles[batch]; ok && status == "pending" {
					c.add("C4", rel, "batch"+batch, "标 pending 但报告文件已存在（%s），状态与产物矛盾", strings.Join(batchFiles[batch], ","))
				}
				continue
			}
			files, ok := batchFiles[batch]
			if !ok || len(files) == 0 {
				c.add("C4", rel, "batch"+batch, "标 done 但找不到匹配 batch%s(.M)*.md 的报告文件（声明区间 %q）——实测批次 11 即以此形态丢失 4 条发现，编排验收未拦住", batch, span)
				continue
			}
			// 条目区间与实际条目集比对
			ids := batchEntries[batch]
			if span != "" && span != "0" {
				if len(ids) == 0 {
					c.add("C4", rel, "batch"+batch, "报告文件 %s 内解析不到任何规范条目——条目须写成 `### [ID] 标题` + `- 字段: 值`，实测多份批次报告自创了 `### [P2] 标题` 格式，导致合并步骤只能靠 LLM 重写条目（违反「合并只做拼接与排序」）", strings.Join(files, ","))
					continue
				}
				for _, want := range parseSpanEndpoints(span) {
					if !containsStr(ids, want) {
						c.add("C4", rel, "batch"+batch, "进度表声明区间含 %s，但报告文件中不存在该条目", want)
					}
				}
			}
			if (span == "" || span == "0") && len(ids) > 0 {
				c.add("C4", rel, "batch"+batch, "进度表未声明条目区间，但报告内有 %d 条", len(ids))
			}
		}
	}
}

// ---------- 辅助 ----------

func (c *checker) rel(p string) string {
	r, err := filepath.Rel(c.root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func parseSpanEndpoints(span string) []string {
	span = strings.NewReplacer("~", " ", "～", " ", "-–", " ").Replace(span)
	var out []string
	for _, f := range strings.Fields(span) {
		f = strings.Trim(f, ",，")
		if idRe.MatchString(f) || adRe.MatchString(f) {
			out = append(out, f)
		}
	}
	return out
}

func splitRow(l string) []string {
	parts := strings.Split(strings.Trim(l, "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func readScope(path string) map[string][]string {
	out := map[string][]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		out[f[0]] = f[1:]
	}
	return out
}

func readBaseline(path string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

func writeBaseline(path string, ps []problem) error {
	var sb strings.Builder
	sb.WriteString("# review-check 存量违规基线（棘轮：只禁增量，不要求存量清零）\n")
	sb.WriteString("# 生成: go run tools/review_check.go -update-baseline\n")
	sb.WriteString("# 收窄纪律同 deadcode 白名单——只许逐条审计后删除，禁止批量填充（见根 CLAUDE.md 与 ADR-0089）\n")
	seen := map[string]bool{}
	for _, p := range ps {
		if seen[p.sig()] {
			continue
		}
		seen[p.sig()] = true
		sb.WriteString(p.sig() + "\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

func joinSet(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "review-check: "+format+"\n", a...)
	os.Exit(2)
}
