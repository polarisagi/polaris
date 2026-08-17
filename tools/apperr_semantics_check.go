//go:build ignore

// apperr_semantics_check 断言 apperr.New/Wrap 的错误码与消息语义一致（L-11 / R2.5）。
//
// 判据：消息里出现 "not found" / "forbidden" / "rate limit" 等语义关键词时，错误码必须是
// 对应的 CodeNotFound / CodeForbidden / CodeResourceExhausted。错配会让 HTTP 层把 404
// 报成 500——apperr.HTTPStatus 只看 Code，不看消息。
//
// 扫描根 2026-08-17 从单 "internal" 扩到全仓三根（ADR-0089 那类失效的复发；
// cmd/ 与 pkg/ 里有 230 处 apperr.New/Wrap 从未被本规则看过）。扩根前已实测 0 新增命中。
//
// 棘轮：存量记在 tools/baselines/apperr-semantics-baseline.md，只禁增量。
package main

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

// codeRules 是「消息关键词 → 应有错误码」的对照表。
// 表驱动而非 if-else 链：新增一类语义只加一行，且顺序即优先级（先匹配先生效）。
var codeRules = []struct {
	keywords []string
	code     string
}{
	{[]string{"rate limit", "quota", "exhausted", "too many"}, "CodeResourceExhausted"},
	{[]string{"not found"}, "CodeNotFound"},
	{[]string{"forbidden", "denied"}, "CodeForbidden"},
}

func main() {
	r := lintutil.NewReporter("apperr-semantics-check", lintutil.LoadBaseline("apperr-semantics-baseline.md"))

	lintutil.Walk(r, lintutil.WalkOptions{IncludeTests: true}, func(f lintutil.File) {
		lintutil.Calls(f.AST, func(call *ast.CallExpr) {
			recv, method, ok := lintutil.SelectorCall(call)
			if !ok || recv != "apperr" || (method != "New" && method != "Wrap") {
				return
			}
			if len(call.Args) < 2 {
				return
			}
			r.Anchor()

			msgLit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || msgLit.Kind != token.STRING {
				return
			}
			msg := strings.ToLower(strings.Trim(msgLit.Value, `"`))

			want, trigger := expectedCode(msg)
			if want == "" {
				return
			}
			got := lintutil.ExprText(call.Args[0])
			if strings.HasSuffix(got, "."+want) || got == want {
				return
			}
			r.Violation(f.At(call), "apperr message 含 %q 应使用 %s 错误码，实际是 %s（违反 L-11 R2.5）",
				trigger, want, got)
		})
	})

	r.RequireAnchors(1, "判据锚在 apperr.New / apperr.Wrap 调用上；若统一错误构造入口改名，请同步本规则")
	r.Done()
}

// expectedCode 返回消息应当对应的错误码与触发它的关键词。
func expectedCode(msgLower string) (code, trigger string) {
	for _, rule := range codeRules {
		for _, kw := range rule.keywords {
			if strings.Contains(msgLower, kw) {
				return rule.code, kw
			}
		}
	}
	return "", ""
}
