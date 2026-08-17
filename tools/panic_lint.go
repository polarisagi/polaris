//go:build ignore

// panic_lint 拦截框架层新增的 panic 调用（F-12 / [E1]，并含 L-04 的导出构造函数子句）。
//
// 规则 [E1]：panic 仅允许出现在 init() 与 cmd/polaris/ 进程入口。库代码里 panic 等于
// 把「这台机器上的一个请求出错」升级成「整个进程死掉」，而 Agent 进程持有的是长驻会话。
//
// 扫描根刻意是 internal + pkg，**不含 cmd/**：cmd/polaris 是进程入口，[E1] 明确允许。
// 这是有据的收窄，别按"统一三根"的直觉改掉。
//
// 豁免：init() 函数体；recover 后原样重抛（panic(r) / panic(err) / panic(rec)）。
// 棘轮：存量记在 tools/baselines/panic-baseline.md，键为 path:line:FuncName。
package main

import (
	"fmt"
	"go/ast"

	"github.com/polarisagi/polaris/tools/lintutil"
)

// rethrowArgs 是 recover 后原样重抛的惯用参数名。按名字放行是个近似：没有类型信息时
// 无法证明这个 r 真的来自 recover()，但收紧到「必须在 defer+recover 块内」会把
// 几处合法的跨函数重抛判成违规——过严的门控最终会被整体关掉，比漏报更糟。
var rethrowArgs = map[string]bool{"r": true, "err": true, "rec": true}

func main() {
	r := lintutil.NewReporter("panic-lint", lintutil.LoadBaseline("panic-baseline.md"))

	lintutil.Walk(r, lintutil.WalkOptions{Roots: []string{"internal", "pkg"}}, func(f lintutil.File) {
		lintutil.FuncDecls(f, func(fn *ast.FuncDecl) {
			if fn.Name.Name == "init" {
				return
			}
			lintutil.Calls(fn.Body, func(call *ast.CallExpr) {
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "panic" {
					return
				}
				r.Anchor()
				if len(call.Args) == 1 {
					if arg, ok := call.Args[0].(*ast.Ident); ok && rethrowArgs[arg.Name] {
						return
					}
				}
				key := fmt.Sprintf("%s:%s", f.At(call), fn.Name.Name)
				if fn.Name.IsExported() && len(fn.Name.Name) > 3 && fn.Name.Name[:3] == "New" {
					r.Violation(key, "导出构造函数 %q 内禁用 panic，请改为返回 error（违反 fail-closed 规范 L-04）",
						fn.Name.Name)
					return
				}
				r.Violation(key, "在非 init() 的框架层函数 %q 内新增 panic() 调用——"+
					"库代码 panic 会把单次请求出错升级成进程退出（违反 [E1] F-12 与棘轮规则）", fn.Name.Name)
			})
		})
	})

	r.RequireAnchors(1, "判据锚在框架层的 panic() 调用上；基线里登记着存量，"+
		"锚点归零说明扫描面而非代码变了")
	r.Done()
}
