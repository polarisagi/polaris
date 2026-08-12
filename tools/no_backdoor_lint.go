//go:build ignore

// no_backdoor_lint 断言 ExecEnvelope.Execute 中 Capability Token 校验逻辑未被窄条件削弱（inv_M7_01）。
//
// 背景：
// M07 §4 规定 CapReadOnly 以上工具必须有能力令牌校验。如果被改成
// `if req.Tool.Capability >= types.CapPrivileged` 则只防特权工具，中等/写入工具无保护。
//
// 本工具扫描 internal/sandbox/envelope.go：
//   1. 确认 Execute 函数体内调用了 RequiresCapabilityToken
//   2. 确认该调用**未被包在** `Capability >= CapPrivileged` 或 `Capability == CapPrivileged` 条件块内
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

			// 检查是否正常调用了 RequiresCapabilityToken
			if call, ok := ifIf.Cond.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "RequiresCapabilityToken" {
					requiresCapTokenCalled = true
				}
			}

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

	fmt.Println("no_backdoor_lint: PASS (1 envelope entry checked)")
}

func nodeToString(fset *token.FileSet, n ast.Node) string {
	var sb strings.Builder
	if err := ast.Fprint(&sb, fset, n, nil); err != nil {
		return ""
	}
	return sb.String()
}
