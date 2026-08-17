//go:build ignore

// must_check_error_lint 拦截关键写操作/外部调用的 error 被 `_ =` 静默丢弃（F-6）。
//
// 关注名单：tools/must-check-error-calls.txt（**规则输入表**，不是抑制表，故留在
// tools/ 下与规则同放，不进 tools/baselines/）。每行一个模式：
//   - `.Method` 前缀点号 → 后缀匹配任意接收者的该方法
//   - `pkg.Func`        → 全等匹配
//
// 豁免：defer 语句内的调用、_test.go、testutil/、以及 Close()/Rollback()
// （这两个的 error 在绝大多数场景下确实无可处置）。
//
// 锚点自毁断言（2026-08-17 新增）：名单里的模式必须在仓库里至少匹配到一次调用——
// 无论那次调用有没有丢弃 error。此前只在**违规时**计数，于是「函数被改名、名单变成
// 一张过期清单」与「代码很干净」在输出上完全一致；这条规则 2026-08-12 就因为
// 计数口径误导过一整轮复核（见下方 Done 输出）。
package main

import (
	"go/ast"
	"os"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

const callListPath = "tools/must-check-error-calls.txt"

// alwaysIgnorable 是 error 确实无可处置的收尾调用。
var alwaysIgnorable = []string{".Close", ".Rollback"}

func main() {
	patterns := loadPatterns()
	r := lintutil.NewReporter("must-check-error-lint", nil) // fail-closed：错误吞没不接受存量

	opts := lintutil.WalkOptions{ExcludeContains: []string{"testutil/"}}
	lintutil.Walk(r, opts, func(f lintutil.File) {
		// 判据面：名单模式匹配到的**全部**调用，不论 error 是否被丢弃。
		lintutil.Calls(f.AST, func(call *ast.CallExpr) {
			if matchAny(lintutil.ExprText(call.Fun), patterns) {
				r.Anchor()
			}
		})

		ast.Inspect(f.AST, func(n ast.Node) bool {
			if _, isDefer := n.(*ast.DeferStmt); isDefer {
				return false // defer 内的调用整体豁免
			}
			assign, ok := n.(*ast.AssignStmt)
			if !ok || !allBlank(assign.Lhs) {
				return true
			}
			for _, rhs := range assign.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				fnText := lintutil.ExprText(call.Fun)
				if !matchAny(fnText, patterns) || hasSuffixAny(fnText, alwaysIgnorable) {
					continue
				}
				r.Violation(f.At(call), "关键调用 %q 的 error 返回值被 _ 静默丢弃"+
					"（违反 F-6 错误吞没防御）", fnText)
			}
			return true
		})
	})

	r.RequireAnchors(1, "判据锚在 "+callListPath+" 列出的调用模式上。一个都匹配不到，"+
		"说明名单已经过期（函数改名/包重构），而不是仓库很干净——请核对名单再谈通过")
	r.Done()
}

func loadPatterns() []string {
	data, err := os.ReadFile(callListPath)
	if err != nil {
		// 名单是规则的判据本身，读不到就没有判据可言，直接判门控失效。
		os.Stderr.WriteString("must-check-error-lint: 无法读取关注名单 " + callListPath + ": " + err.Error() + "\n")
		os.Exit(2)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		os.Stderr.WriteString("must-check-error-lint: 关注名单 " + callListPath + " 为空，规则没有判据\n")
		os.Exit(2)
	}
	return out
}

func matchAny(fnText string, patterns []string) bool {
	if fnText == "" {
		return false
	}
	for _, p := range patterns {
		if strings.HasPrefix(p, ".") {
			if strings.HasSuffix(fnText, p) {
				return true
			}
			continue
		}
		if fnText == p {
			return true
		}
	}
	return false
}

func hasSuffixAny(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

func allBlank(lhs []ast.Expr) bool {
	if len(lhs) == 0 {
		return false
	}
	for _, e := range lhs {
		if id, ok := e.(*ast.Ident); !ok || id.Name != "_" {
			return false
		}
	}
	return true
}
