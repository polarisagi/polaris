//go:build ignore

// regex_greedy_check 拦截贪婪跨行正则 `(?s).*`（L-12）。
//
// 判据：regexp.MustCompile / Compile 的字面量模式同时含 `(?s)` 与 `.*` 时报红。
// 这类模式在长文本上会一路吃到最后一个匹配，典型后果是解析器把两段无关内容并成一段。
// 正解通常是括号计数扫描或非贪婪 `.*?`。
//
// 扫描根 2026-08-17 从单 "internal" 扩到全仓三根（ADR-0089）。扩根前已实测 0 新增命中。
//
// 抑制：棘轮基线 regex-greedy-baseline.md（按 path:line 逐条）+ 整文件豁免
// regex-greedy-allowlist.txt。两张表 2026-08-17 从 scripts/ 归位到 tools/baselines/——
// 此前同一条规则的两张抑制表分居两个目录，没人说得清它一共放过了多少。
package main

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

func main() {
	r := lintutil.NewReporter("regex-greedy-check", lintutil.LoadBaseline("regex-greedy-baseline.md"))
	fileAllow := lintutil.LoadBaseline("regex-greedy-allowlist.txt")

	lintutil.Walk(r, lintutil.WalkOptions{IncludeTests: true}, func(f lintutil.File) {
		if fileAllow.Has(f.Path) {
			return
		}
		lintutil.Calls(f.AST, func(call *ast.CallExpr) {
			recv, method, ok := lintutil.SelectorCall(call)
			if !ok || recv != "regexp" || (method != "MustCompile" && method != "Compile") {
				return
			}
			if len(call.Args) < 1 {
				return
			}
			r.Anchor()

			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return
			}
			pattern := strings.Trim(lit.Value, "\"`")
			if !strings.Contains(pattern, "(?s)") || !strings.Contains(pattern, ".*") {
				return
			}
			r.Violation(f.At(call), "贪婪跨行正则 (?s).* 可能导致匹配过多，"+
				"建议改用括号计数扫描或非贪婪 .*?（违反 L-12）")
		})
	})

	r.RequireAnchors(1, "判据锚在 regexp.MustCompile / regexp.Compile 调用上")
	r.Done()
}
