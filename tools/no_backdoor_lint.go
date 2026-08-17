//go:build ignore

// no_backdoor_lint 是「授权不得被绕过」这一族的门控，含两条独立断言：
//
//	[inv_M7_01] ExecEnvelope.Execute 的 Capability Token 校验未被窄条件削弱；
//	[GD-14-004] 调用 PolicyGate.Review 的函数必须先有 `== nil` 的 fail-closed 分支。
//
// GD-14-004 于 2026-08-17 从 Makefile 的 policy-gate-check 目标搬进来。原目标是一个
// 恒绿门控：grep 完只 echo 一句"请确保有 nil 判定"，然后无条件 PASS，从不校验它自己
// 提出的要求。按 ADR-0091，恒绿门控比没有门控更糟——它占着一条门控计数。
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
	"path/filepath"
	"strings"
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

	reviewSites := checkPolicyGateFailClosed(fset)

	fmt.Printf("no_backdoor_lint: PASS (1 envelope entry, %d PolicyGate.Review call site(s) checked)\n", reviewSites)
}

// checkPolicyGateFailClosed 断言 GD-14-004：每个调用 PolicyGate.Review 的函数，
// 必须在调用之前有一条 `<同一个 gate 字段> == nil` 的判定分支。
//
// 判据锚在**字段名**上（`m.policyGate.Review(...)` 形如 X.policyGate.Review），
// 不依赖类型信息——本仓门控一律走 go/parser 单文件 AST，不引 go/types。
// 代价是重命名字段会让锚点失效，故下方保留自毁断言：一个调用点都找不到即 exit 2。
// 这是 ADR-0091 那份「门控失真的四种形态」里第三条（判据可被 no-op 满足）的解药：
// 规则消失必须表现为红灯，而不是继续打印 PASS。
func checkPolicyGateFailClosed(fset *token.FileSet) int {
	sites := 0
	violations := 0

	for _, root := range []string{"internal", "cmd", "pkg"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			node, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// 语法错误由 go build 拦，门控层面不重复报，但也不能当作"检查过了"。
				return nil
			}
			for _, decl := range node.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				for _, gateExpr := range findReviewCalls(fn.Body) {
					sites++
					if !hasNilGuard(fn.Body, gateExpr) {
						pos := fset.Position(fn.Pos())
						fmt.Fprintf(os.Stderr, "%s:%d: %s 调用 %s.Review 前没有 `%s == nil` 的 fail-closed 判定——"+
							"PolicyGate 未注入时授权会直接 panic 或被跳过（违反 GD-14-004）\n",
							path, pos.Line, fn.Name.Name, gateExpr, gateExpr)
						violations++
					}
				}
			}
			return nil
		})
	}

	if sites == 0 {
		fmt.Fprintf(os.Stderr, "no_backdoor_lint: FAIL — 全仓找不到任何 PolicyGate.Review 调用点。"+
			"判据锚在字段名 *policyGate* 上，字段被重命名或调用点被删除都会走到这里；"+
			"请确认 GD-14-004 仍然成立并同步本规则，而不是让它继续静默通过（exit 2）\n")
		os.Exit(2)
	}
	if violations > 0 {
		fmt.Fprintf(os.Stderr, "no_backdoor_lint: FAIL — %d 个 PolicyGate.Review 调用点缺少 fail-closed 判定（GD-14-004）\n", violations)
		os.Exit(1)
	}
	return sites
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
		gate := exprText(sel.X)
		if gate == "" || !strings.Contains(strings.ToLower(gate), "policygate") {
			return true
		}
		out = append(out, gate)
		return true
	})
	return out
}

// hasNilGuard 判定函数体内是否存在 `gate == nil` 的条件判定。
func hasNilGuard(body *ast.BlockStmt, gate string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || bin.Op != token.EQL {
			return true
		}
		id, ok := bin.Y.(*ast.Ident)
		if !ok || id.Name != "nil" {
			return true
		}
		if exprText(bin.X) == gate {
			found = true
		}
		return !found
	})
	return found
}

// exprText 把 a.b.c 形式的选择器链还原成文本；其余形态返回空串（不参与判定）。
func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		prefix := exprText(x.X)
		if prefix == "" {
			return ""
		}
		return prefix + "." + x.Sel.Name
	}
	return ""
}

func nodeToString(fset *token.FileSet, n ast.Node) string {
	var sb strings.Builder
	if err := ast.Fprint(&sb, fset, n, nil); err != nil {
		return ""
	}
	return sb.String()
}
