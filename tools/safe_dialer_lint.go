//go:build ignore

// safe_dialer_lint 扫描裸出站网络连接，防止绕过 SSRFGuard（inv_safe_dialer_01）。
//
// 扫描特征（F-1 落地 2026-08-12）：
//   - websocket.Dialer{ 且在同函数内无 NetDialContext 赋值/声明 → ERROR
//   - net.Dial / net.DialTimeout
//   - http.Get / http.Post / http.PostForm
//   - grpc.Dial / grpc.NewClient
//   - smtp.SendMail / smtp.Dial
//
// 豁免：
//   - internal/security/network/ 包自身
//   - _test.go 文件
//
// 扫描根：internal/ + cmd/ + pkg/
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

var errCount int
var fileCount int

func main() {
	roots := []string{"internal", "cmd", "pkg"}
	fset := token.NewFileSet()

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.Contains(path, "internal/security/network/") {
				return nil
			}

			fileCount++
			checkFile(fset, path)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "safe_dialer_lint: walk error on %s: %v\n", root, err)
			os.Exit(2)
		}
	}

	fmt.Printf("safe_dialer_lint: scanned %d files\n", fileCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "safe_dialer_lint: FAIL — %d violation(s) found\n", errCount)
		os.Exit(1)
	}
	fmt.Println("safe_dialer_lint: PASS")
}

func checkFile(fset *token.FileSet, path string) {
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "safe_dialer_lint: parse error in %s: %v\n", path, err)
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		checkFunction(fset, path, fn)
		return true
	})
}

func checkFunction(fset *token.FileSet, path string, fn *ast.FuncDecl) {
	wsDialers := make(map[string]token.Pos)
	netDialGuarded := make(map[string]bool)

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range stmt.Rhs {
				if isWebsocketDialerLit(rhs) {
					if i < len(stmt.Lhs) {
						if ident, ok := stmt.Lhs[i].(*ast.Ident); ok && ident.Name != "_" {
							wsDialers[ident.Name] = stmt.Pos()
							if compositeLitHasNetDialContext(rhs) {
								netDialGuarded[ident.Name] = true
							}
						}
					}
				}
			}
			for _, lhs := range stmt.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "NetDialContext" {
						if ident, ok := sel.X.(*ast.Ident); ok {
							netDialGuarded[ident.Name] = true
						}
					}
				}
			}

		case *ast.ValueSpec:
			for i, value := range stmt.Values {
				if isWebsocketDialerLit(value) {
					if i < len(stmt.Names) {
						wsDialers[stmt.Names[i].Name] = stmt.Pos()
						if compositeLitHasNetDialContext(value) {
							netDialGuarded[stmt.Names[i].Name] = true
						}
					}
				}
			}

		case *ast.CallExpr:
			checkBareCall(fset, path, stmt)
		}
		return true
	})

	for varName, pos := range wsDialers {
		if !netDialGuarded[varName] {
			position := fset.Position(pos)
			fmt.Printf("%s:%d: websocket.Dialer 变量 %q 缺少 NetDialContext 注入（违反 inv_safe_dialer_01）\n",
				path, position.Line, varName)
			errCount++
		}
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
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "websocket" && sel.Sel.Name == "Dialer"
}

func compositeLitHasNetDialContext(expr ast.Expr) bool {
	if u, ok := expr.(*ast.UnaryExpr); ok {
		expr = u.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if ok && key.Name == "NetDialContext" {
			return true
		}
	}
	return false
}

func checkBareCall(fset *token.FileSet, path string, call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}

	pkgName := pkg.Name
	fnName := sel.Sel.Name
	pos := fset.Position(call.Pos())

	if pkgName == "net" && (fnName == "Dial" || fnName == "DialTimeout") {
		fmt.Printf("%s:%d: 裸 net.%s() 调用，必须通过 SafeDialer.DialContext (inv_safe_dialer_01)\n",
			path, pos.Line, fnName)
		errCount++
	}
	if pkgName == "http" && (fnName == "Get" || fnName == "Post" || fnName == "PostForm") {
		fmt.Printf("%s:%d: 裸 http.%s() 使用 DefaultClient，需注入 SafeDialer Transport (inv_safe_dialer_01)\n",
			path, pos.Line, fnName)
		errCount++
	}
	if pkgName == "grpc" && (fnName == "Dial" || fnName == "NewClient") {
		fmt.Printf("%s:%d: 裸 grpc.%s() 调用，需通过 SafeDialer 注入 (inv_safe_dialer_01)\n",
			path, pos.Line, fnName)
		errCount++
	}
	if pkgName == "smtp" && (fnName == "SendMail" || fnName == "Dial") {
		fmt.Printf("%s:%d: 裸 smtp.%s() 调用，需通过 SafeDialer 注入 (inv_safe_dialer_01)\n",
			path, pos.Line, fnName)
		errCount++
	}
}
