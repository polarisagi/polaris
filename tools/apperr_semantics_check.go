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
//
// 2026-09-20 追加三条同属"apperr 语义误用"的判据（来源 lint-backlog GR-9.2-003 /
// GR-10.2-001 / GR-5.2-010 / GR-6.2 系列）：
//   - L-11b：禁止以 CodeOK 构造错误。返回非 nil error 表达"成功"会被调用方
//     `if err != nil` 当异常处理（webhook 握手曾因此向已写出的响应追加 500 错误体）；
//   - L-11c：禁止以 apperr.New(..., "log event") 伪造错误充当日志属性——它掩盖了
//     本应返回给调用方的真实错误（渠道适配器配置缺失时静默 return nil）；
//   - L-11d：errors.Is 的目标不得是 apperr.New/Wrap 构造的包级变量。*Error.Is 按 Code
//     比较，同码的任意错误都会命中（ErrReplanExhausted 曾吞掉 Provider 限流错误）；
//     包级哨兵须用 apperr.NewSentinel（按身份比较）或 errors.New。
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

	// 第一遍：收集以 apperr.New/Wrap 初始化的包级变量（按变量名，供 L-11d 匹配）。
	codeSentinels := map[string]bool{}
	lintutil.Walk(r, lintutil.WalkOptions{}, func(f lintutil.File) {
		for _, decl := range f.AST.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if call, ok := vs.Values[i].(*ast.CallExpr); ok {
						if recv, method, ok := lintutil.SelectorCall(call); ok && recv == "apperr" && (method == "New" || method == "Wrap") {
							codeSentinels[name.Name] = true
						}
					}
				}
			}
		}
	})

	lintutil.Walk(r, lintutil.WalkOptions{IncludeTests: true}, func(f lintutil.File) {
		isTest := strings.HasSuffix(f.Path, "_test.go")
		lintutil.Calls(f.AST, func(call *ast.CallExpr) {
			recv, method, ok := lintutil.SelectorCall(call)
			if ok && recv == "errors" && method == "Is" && len(call.Args) == 2 && !isTest {
				r.Anchor()
				name := ""
				switch t := call.Args[1].(type) {
				case *ast.Ident:
					name = t.Name
				case *ast.SelectorExpr:
					name = t.Sel.Name
				}
				if name != "" && codeSentinels[name] {
					r.Violation(f.At(call), "errors.Is 目标 %s 由 apperr.New/Wrap 构造，按 Code 匹配会误中同码错误；改用 apperr.NewSentinel（L-11d）", name)
				}
				return
			}
			if !ok || recv != "apperr" || (method != "New" && method != "Wrap") {
				return
			}
			if len(call.Args) < 2 {
				return
			}
			r.Anchor()
			if code := lintutil.ExprText(call.Args[0]); strings.HasSuffix(code, "CodeOK") {
				r.Violation(f.At(call), "禁止以 CodeOK 构造错误：非 nil error 会被调用方当作失败处理（L-11b）")
			}
			if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Trim(lit.Value, `"`) == "log event" {
				r.Violation(f.At(call), `禁止 apperr.New(..., "log event") 伪造错误充当日志属性：应返回真实错误或直接记录上下文（L-11c）`)
			}

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
