//go:build ignore

// safe_dialer_lint 拦截绕过 SafeDialer 的裸出站连接（inv_safe_dialer_01 / F-1 / L-01）。
//
// 出站连接必须经 SafeDialer，否则 SSRFGuard 的私网/环回拦截形同虚设——攻击者只要让
// Agent 去请求一个 169.254.169.254 之类的地址就能读到云元数据。
//
// 判据三类：
//  1. 裸调用：net.Dial / net.DialContext / net.DialTimeout、http.Get/Post/Head/PostForm、
//     grpc.Dial / grpc.NewClient、smtp.SendMail / smtp.Dial；
//  2. 裸**引用**：http.DefaultClient / http.DefaultTransport（不必是调用——把
//     DefaultClient 赋给字段同样绕过 SafeDialer，只查 CallExpr 会漏掉这一整类）；
//  3. websocket.Dialer 复合字面量在同函数内未注入 NetDialContext。
//
// 豁免：internal/security/network/ 包自身（SafeDialer 的实现处）与 _test.go。
//
// ── 2026-08-17：与 internal/lint 的重复判据合并 ──
//
// 本仓有两套静态分析：tools/（make lint，AST 单文件扫描）与 internal/lint/（go test，
// 不变量测试）。二者的**判据命名空间此前是重叠的**，出站连接这条正是撞在一起的：
// internal/lint/inv_lint_test.go 的 Test_inv_M11_05_NoRawNetDial 与
// Test_inv_M1_01_NoRawHTTPCalls 与本规则查的是同一件事，而且**已经漂了**——
//
//	本规则独有：net.DialTimeout、http.PostForm、smtp.*、websocket.Dialer、扫 cmd/
//	那两条独有：net.DialContext、http.Head、http.DefaultClient 的非调用引用
//
// 也就是说两边各自放过了对方能抓的形态，而任何一方绿灯都会被读成"这条不变量守住了"。
// 现取并集归本规则单一所有（新增形态全仓 0 命中，合并零成本），那两条测试改为指向注记。
// 判据只能有一个归属：两份实现不会同步演进，只会各自腐烂到互相掩盖。
package main

import (
	"go/ast"

	"github.com/polarisagi/polaris/tools/lintutil"
)

// bareCalls 是「包名 → 禁用函数集」的对照表，附一句为什么这个入口危险。
var bareCalls = map[string]struct {
	fns    map[string]bool
	reason string
}{
	"net":  {map[string]bool{"Dial": true, "DialContext": true, "DialTimeout": true}, "必须通过 SafeDialer.DialContext"},
	"http": {map[string]bool{"Get": true, "Post": true, "Head": true, "PostForm": true}, "走的是 DefaultClient，需注入 SafeDialer Transport"},
	"grpc": {map[string]bool{"Dial": true, "NewClient": true}, "需通过 SafeDialer 注入"},
	"smtp": {map[string]bool{"SendMail": true, "Dial": true}, "需通过 SafeDialer 注入"},
}

// bareRefs 是禁止**引用**（不必是调用）的包成员：把 http.DefaultClient 赋给一个字段
// 或作为参数传出去，同样绕过 SafeDialer，而只查 CallExpr 看不见这一类。
var bareRefs = map[string]map[string]bool{
	"http": {"DefaultClient": true, "DefaultTransport": true},
}

func main() {
	// 棘轮：合并两套系统的判据后，http.DefaultTransport 的非调用引用首次进入判据面，
	// 一次暴露 4 处存量（1 处刻意豁免 + 3 处 nil 回落）。逐条理由见基线文件。
	r := lintutil.NewReporter("safe-dialer-lint", lintutil.LoadBaseline("safe-dialer-baseline.md"))

	opts := lintutil.WalkOptions{ExcludeContains: []string{"internal/security/network/"}}
	lintutil.Walk(r, opts, func(f lintutil.File) {
		lintutil.Calls(f.AST, func(call *ast.CallExpr) {
			pkg, fn, ok := lintutil.SelectorCall(call)
			if !ok {
				return
			}
			entry, watched := bareCalls[pkg]
			if !watched || !entry.fns[fn] {
				return
			}
			r.Anchor()
			r.Violation(f.At(call), "裸 %s.%s() 调用，%s（违反 inv_safe_dialer_01）", pkg, fn, entry.reason)
		})

		// 裸引用：http.DefaultClient / DefaultTransport，不必是调用。
		ast.Inspect(f.AST, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if !bareRefs[pkg.Name][sel.Sel.Name] {
				return true
			}
			r.Anchor()
			r.Violation(f.At(sel), "裸引用 %s.%s——它承载的是全局默认 Transport，"+
				"任何经它发出的请求都不过 SSRFGuard（违反 inv_safe_dialer_01）", pkg.Name, sel.Sel.Name)
			return true
		})

		lintutil.FuncDecls(f, func(fd *ast.FuncDecl) {
			checkWebsocketDialers(r, f, fd)
		})
	})

	r.RequireAnchors(0, "本规则 fail-closed 且判据面理应为空，故不设最小锚点数；"+
		"扫描面本身由 lintutil.Walk 的扫描根存在性检查兜底")
	r.Done()
}

// checkWebsocketDialers 找出未注入 NetDialContext 的 websocket.Dialer 变量。
//
// 按函数为单位收集：字面量里直接给 NetDialContext、或事后 `d.NetDialContext = ...` 赋值
// 都算已注入。跨函数传递不追（无类型信息），宁可漏报不误报。
func checkWebsocketDialers(r *lintutil.Reporter, f lintutil.File, fn *ast.FuncDecl) {
	declared := map[string]ast.Node{}
	guarded := map[string]bool{}

	record := func(name string, value ast.Expr, at ast.Node) {
		if name == "" || name == "_" || !isWebsocketDialerLit(value) {
			return
		}
		declared[name] = at
		if compositeHasField(value, "NetDialContext") {
			guarded[name] = true
		}
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range stmt.Rhs {
				if i < len(stmt.Lhs) {
					if id, ok := stmt.Lhs[i].(*ast.Ident); ok {
						record(id.Name, rhs, stmt)
					}
				}
			}
			// 事后赋值：d.NetDialContext = safeDialer.DialContext
			for _, lhs := range stmt.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "NetDialContext" {
					if id, ok := sel.X.(*ast.Ident); ok {
						guarded[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range stmt.Values {
				if i < len(stmt.Names) {
					record(stmt.Names[i].Name, v, stmt)
				}
			}
		}
		return true
	})

	for name, at := range declared {
		r.Anchor()
		if guarded[name] {
			continue
		}
		r.Violation(f.At(at), "websocket.Dialer 变量 %q 缺少 NetDialContext 注入，"+
			"该连接不经 SSRFGuard（违反 inv_safe_dialer_01）", name)
	}
}

func isWebsocketDialerLit(expr ast.Expr) bool {
	if u, ok := expr.(*ast.UnaryExpr); ok {
		expr = u.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	return lintutil.ExprText(lit.Type) == "websocket.Dialer"
}

func compositeHasField(expr ast.Expr, field string) bool {
	if u, ok := expr.(*ast.UnaryExpr); ok {
		expr = u.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
				return true
			}
		}
	}
	return false
}
