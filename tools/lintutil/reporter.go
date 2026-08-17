package lintutil

import (
	"fmt"
	"io"
	"os"
)

// Reporter 统一门控的报告与退出码语义：
//
//	exit 0 —— 通过（可能有被基线抑制的存量，会在结尾计数打印）
//	exit 1 —— 发现新增违规
//	exit 2 —— **门控自身失效**：锚点不见了、扫描根不存在、源文件解析不了
//
// 把 2 与 1 分开是刻意的。1 说"代码有问题"，2 说"这条规则已经不知道自己在看什么"，
// 二者要求的动作完全不同：前者改代码，后者改规则。此前多数工具把后者写成静默
// return 或 continue，于是规则失效表现为一片绿灯（ADR-0091「门控失真的四种形态」）。
type Reporter struct {
	name     string
	baseline KeySet

	out io.Writer
	err io.Writer

	violations   int
	suppressed   int
	scanned      int
	anchors      int
	parseErrors  int
	fatalMessage string
}

// NewReporter 创建一个报告器。name 会出现在每一行输出的前缀里。
// baseline 传 nil 表示这条规则 fail-closed、不接受存量。
func NewReporter(name string, baseline KeySet) *Reporter {
	return &Reporter{name: name, baseline: baseline, out: os.Stdout, err: os.Stderr}
}

// Violation 报告一条违规。key 为 "path:line" 形式；命中基线则记为抑制而不报红。
func (r *Reporter) Violation(key, format string, args ...any) {
	if r.baseline.Has(key) {
		r.suppressed++
		return
	}
	r.violations++
	fmt.Fprintf(r.err, "%s: %s\n", key, fmt.Sprintf(format, args...))
}

// Anchor 记录一次「判据面命中」。规则应当在每个进入判定的候选点上调用它——
// **包括判定通过的那些**，使 RequireAnchors 能在判据面整体消失时报红而不是打印 PASS。
func (r *Reporter) Anchor() { r.anchors++ }

// Anchors 一次记录 n 次判据面命中。
func (r *Reporter) Anchors(n int) { r.anchors += n }

// AnchorCount 返回当前累计的判据面命中数。
func (r *Reporter) AnchorCount() int { return r.anchors }

// Violations 返回已报告的新增违规数。供「一个工具里挂多条断言、其中一部分尚未迁到
// Reporter」的过渡期使用：这类工具需要把两边的失败合并成一个退出码。
func (r *Reporter) Violations() int { return r.violations }

// AddViolations 把工具自行统计的违规数并入报告器，使 Done 的退出码涵盖它们。
func (r *Reporter) AddViolations(n int) { r.violations += n }

// RequireAnchors 是锚点自毁断言：判据面命中数低于 min 即判定规则本身已失效（exit 2）。
//
// 没有这一条时，"字段改名 / 文件搬走 / 调用形态变了"与"仓库很干净"在门控输出上
// 长得一模一样。hint 应当说明这条规则锚在什么上、失效时该去改哪里。
func (r *Reporter) RequireAnchors(min int, hint string) {
	if r.anchors < min {
		r.Fatalf("判据面命中 %d 处（要求至少 %d 处）——规则已失效而非仓库干净。%s",
			r.anchors, min, hint)
	}
}

// ParseFailure 记录一次源文件解析失败。
func (r *Reporter) ParseFailure(path string, err error) {
	r.parseErrors++
	fmt.Fprintf(r.err, "%s: 解析失败，本文件未被 %s 检查: %v\n", path, r.name, err)
}

// Fatalf 记录门控自身失效，Done 时以 exit 2 结束。
func (r *Reporter) Fatalf(format string, args ...any) {
	if r.fatalMessage == "" {
		r.fatalMessage = fmt.Sprintf(format, args...)
	}
}

// Done 打印汇总并按上述语义退出。规则的 main 应以 defer 之外的形式在末尾调用它。
func (r *Reporter) Done() {
	if r.fatalMessage != "" {
		fmt.Fprintf(r.err, "%s: FAIL(门控失效) — %s\n", r.name, r.fatalMessage)
		os.Exit(2)
	}
	if r.parseErrors > 0 {
		fmt.Fprintf(r.err, "%s: FAIL(门控失效) — %d 个文件解析失败，其内容未经检查\n", r.name, r.parseErrors)
		os.Exit(2)
	}
	if r.violations > 0 {
		fmt.Fprintf(r.err, "%s: FAIL — %d 处新增违规（另有 %d 处存量在基线内）\n",
			r.name, r.violations, r.suppressed)
		os.Exit(1)
	}
	fmt.Fprintf(r.out, "%s: PASS（扫描 %d 文件，判据面 %d 处，基线抑制 %d 处）\n",
		r.name, r.scanned, r.anchors, r.suppressed)
}
