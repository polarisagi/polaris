//go:build ignore

// no_backdoor_lint 是「授权不得被绕过」这一族的门控，含两条独立断言：
//
//	[inv_M7_01] ExecEnvelope.Execute 的 Capability Token 校验未被窄条件削弱；
//	[L-17] 调用 PolicyGate.Review 的函数必须先有 `== nil` 的 fail-closed 分支。
//
// L-17 于 2026-08-17 从 Makefile 的 policy-gate-check 目标搬进来。原目标是一个
// 恒绿门控：grep 完只 echo 一句"请确保有 nil 判定"，然后无条件 PASS，从不校验它自己
// 提出的要求。按 ADR-0091，恒绿门控比没有门控更糟——它占着一条门控计数。
//
// 编号同轮由 GD-14-004 改为 L-17：GD 是**批次内序号、跨轮复用**（review_check.go
// §GD 编号判定处写明），却被当成常驻门控 ID 用了三处——taint 裸构造、FSM 控制流、
// 本条，三条互不相干的断言共用一个编号，而 lint_selftest 正是按 ID 数门控条数的。
// 常驻门控只能用稳定命名空间（F-* / L-* / inv_*）。
//
// ── 以下为 inv_M7_01 的原始说明 ──
//
// no_backdoor_lint 断言 ExecEnvelope.Execute 中 Capability Token 校验逻辑未被窄条件削弱（inv_M7_01）。
//
// 背景：
// M07 §4 规定 CapReadOnly 以上工具必须有能力令牌校验。如果被改成
// `if req.Tool.Capability >= types.CapPrivileged` 则只防特权工具，中等/写入工具无保护。
//
// 本工具扫描 internal/sandbox/envelope.go：
//  1. 确认 Execute 函数体内调用了 RequiresCapabilityToken
//  2. 确认该调用**未被包在** `Capability >= CapPrivileged` 或 `Capability == CapPrivileged` 条件块内
//
// 负向验证：将 RequiresCapabilityToken 改回 `>= CapPrivileged` 条件 → 本门控报红。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

func main() {
	targetFile := "internal/sandbox/envelope.go"
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, targetFile, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no_backdoor_lint: parse error in %s: %v\n", targetFile, err)
		os.Exit(2)
	}

	execFuncFound := false
	requiresCapTokenCalled := false
	gatedByPrivilegedOnly := false

	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Execute" || fn.Recv == nil {
			return true
		}
		// 找到 ExecEnvelope.Execute
		execFuncFound = true

		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			// 查 if 条件
			ifIf, ok := inner.(*ast.IfStmt)
			if !ok {
				return true
			}

			// 检查条件表达式是否包含 CapPrivileged
			condStr := nodeToString(fset, ifIf.Cond)
			if strings.Contains(condStr, "CapPrivileged") {
				// 如果 CapPrivileged 条件块内部包含了 RequiresCapabilityToken
				ast.Inspect(ifIf.Body, func(bodyNode ast.Node) bool {
					if call, ok := bodyNode.(*ast.CallExpr); ok {
						if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "RequiresCapabilityToken" {
							gatedByPrivilegedOnly = true
						}
					}
					return true
				})
			}

			// 检查是否正常调用了 RequiresCapabilityToken（可能与 && 条件组合）
			ast.Inspect(ifIf.Cond, func(condNode ast.Node) bool {
				if call, ok := condNode.(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "RequiresCapabilityToken" {
						requiresCapTokenCalled = true
					}
				}
				return true
			})

			return true
		})

		return true
	})

	if !execFuncFound {
		fmt.Fprintf(os.Stderr, "no_backdoor_lint: ExecEnvelope.Execute function not found in %s\n", targetFile)
		os.Exit(1)
	}

	if !requiresCapTokenCalled {
		fmt.Fprintf(os.Stderr, "no_backdoor_lint: FAIL — RequiresCapabilityToken call missing in ExecEnvelope.Execute (inv_M7_01)\n")
		os.Exit(1)
	}

	if gatedByPrivilegedOnly {
		fmt.Fprintf(os.Stderr, "no_backdoor_lint: FAIL — RequiresCapabilityToken is wrongly guarded by CapPrivileged condition (inv_M7_01)\n")
		os.Exit(1)
	}

	fmt.Println("no_backdoor_lint: inv_M7_01 PASS（1 个 envelope 入口）")
	checkPolicyGateFailClosed() // 内部以 Reporter.Done 收尾并决定退出码
}

// checkPolicyGateFailClosed 断言 L-17：每个调用 PolicyGate.Review 的函数，
// 必须在调用之前有一条 `<同一个 gate 字段> == nil` 的判定分支。
//
// 判据锚在**字段名**上（`m.policyGate.Review(...)` 形如 X.policyGate.Review），
// 不依赖类型信息——本仓门控一律走 go/parser 单文件 AST，不引 go/types。
// 代价是重命名字段会让锚点失效，故下方保留自毁断言：一个调用点都找不到即 exit 2。
// 这是 ADR-0091 那份「门控失真的四种形态」里第三条（判据可被 no-op 满足）的解药：
// 规则消失必须表现为红灯，而不是继续打印 PASS。
func checkPolicyGateFailClosed() {
	r := lintutil.NewReporter("no-backdoor-lint(L-17)", nil) // fail-closed：授权入口不接受存量

	lintutil.Walk(r, lintutil.WalkOptions{}, func(f lintutil.File) {
		lintutil.FuncDecls(f, func(fn *ast.FuncDecl) {
			for _, gate := range findReviewCalls(fn.Body) {
				r.Anchor()
				if lintutil.HasNilGuard(fn.Body, gate) {
					continue
				}
				r.Violation(f.At(fn), "%s 调用 %s.Review 前没有 `%s == nil` 的 fail-closed 判定——"+
					"PolicyGate 未注入时授权会直接 panic 或被整段跳过（违反 L-17）",
					fn.Name.Name, gate, gate)
			}
		})
	})

	r.RequireAnchors(1, "判据锚在字段名含 policyGate 的 .Review(...) 调用上；"+
		"字段改名或调用点被删都会走到这里——请确认 L-17 仍然成立并同步本规则")
	r.Done()
}

// findReviewCalls 返回函数体内所有 `<x>.policyGate.Review(...)` 调用的 gate 表达式文本。
func findReviewCalls(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Review" {
			return true
		}
		gate := lintutil.ExprText(sel.X)
		if gate == "" || !strings.Contains(strings.ToLower(gate), "policygate") {
			return true
		}
		out = append(out, gate)
		return true
	})
	return out
}

func nodeToString(fset *token.FileSet, n ast.Node) string {
	var sb strings.Builder
	if err := ast.Fprint(&sb, fset, n, nil); err != nil {
		return ""
	}
	return sb.String()
}
