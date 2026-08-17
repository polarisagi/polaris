//go:build ignore

// safe_dialer_lint 拦截绕过 SafeDialer 的裸出站连接（inv_safe_dialer_01 / F-1 / L-01）。
//
// 出站连接必须经 SafeDialer，否则 SSRFGuard 的私网/环回拦截形同虚设——攻击者只要让
// Agent 去请求一个 169.254.169.254 之类的地址就能读到云元数据。
//
// 判据两类：
//  1. 裸调用：net.Dial / net.DialTimeout、http.Get/Post/PostForm（走 DefaultClient）、
//     grpc.Dial / grpc.NewClient、smtp.SendMail / smtp.Dial；
//  2. websocket.Dialer 复合字面量在同函数内未注入 NetDialContext。
//
// 豁免：internal/security/network/ 包自身（SafeDialer 的实现处）与 _test.go。
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
	"net":  {map[string]bool{"Dial": true, "DialTimeout": true}, "必须通过 SafeDialer.DialContext"},
	"http": {map[string]bool{"Get": true, "Post": true, "PostForm": true}, "走的是 DefaultClient，需注入 SafeDialer Transport"},
	"grpc": {map[string]bool{"Dial": true, "NewClient": true}, "需通过 SafeDialer 注入"},
	"smtp": {map[string]bool{"SendMail": true, "Dial": true}, "需通过 SafeDialer 注入"},
}

func main() {
	r := lintutil.NewReporter("safe-dialer-lint", nil) // fail-closed：出站边界不接受存量

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
