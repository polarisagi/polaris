//go:build ignore

// fsm_io_lint 扫描 internal/agent/fsm/ 中 Transition.Effects 闭包内持锁同步 IO 调用（F-4）。
//
// 展开深度：Effects 闭包体 + 它直接调用的同包函数/方法体（一层调用图展开）。
//
//	2026-08-12：此前只扫闭包体本身，对 B-1 的真实形态完全失明——
//	Effects → sm.trySystem1Bypass(sCtx) → sm.skillMatcher.MatchIntent(...)，
//	IO 藏在被调方法里。负向验证（把 MatchIntent 调用塞回 trySystem1Bypass）当时
//	仍报 PASS，等于这条门控从未防住它要防的那个缺陷。
//
// 已知局限：只展开一层。A→B→C 的深层 IO 抓不到，依赖 code review。这是成本/收益的
// 折中——展开全图需要类型信息，而 B-1 这类「闭包直接调一个私有 helper」是实际发生过
// 的形态，一层已覆盖。不要把本工具当作完备性保证。
//
// 外部名单：tools/fsm-io-denylist.txt
//
// 负向验证：把 fsm/transitions.go 的 trySystem1Bypass 改回直接调 sm.skillMatcher.MatchIntent
// → 本门控须报红。
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

var errCount int
var closureCount int

func main() {
	denylistPath := "tools/fsm-io-denylist.txt"
	denylist, err := loadDenylist(denylistPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsm_io_lint: load denylist %s: %v\n", denylistPath, err)
		os.Exit(2)
	}

	targetDir := "internal/agent/fsm"
	fset := token.NewFileSet()

	var files []string
	err = filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsm_io_lint: walk %s: %v\n", targetDir, err)
		os.Exit(2)
	}

	// 先建同包函数/方法索引，供 Effects 闭包做一层调用图展开。
	pkgFuncs := indexPackageFuncs(fset, files)
	for _, path := range files {
		checkFile(fset, path, denylist, pkgFuncs)
	}

	fmt.Printf("fsm_io_lint: scanned %d Effects closure(s), %d package func(s) indexed for 1-hop expansion\n",
		closureCount, len(pkgFuncs))
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "fsm_io_lint: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("fsm_io_lint: PASS")
}

func loadDenylist(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	list := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		list[line] = true
	}
	return list, scanner.Err()
}

// pkgFunc 记录同包函数/方法的函数体与其所在文件，供一层调用图展开使用。
type pkgFunc struct {
	body *ast.BlockStmt
	path string
}

// indexPackageFuncs 按函数名索引同包所有顶层函数与方法（方法只按方法名索引，
// 不区分 receiver 类型——fsm 包内方法名不重复，够用且避免引入类型检查依赖）。
func indexPackageFuncs(fset *token.FileSet, files []string) map[string]pkgFunc {
	idx := make(map[string]pkgFunc)
	for _, path := range files {
		node, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range node.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			idx[fd.Name.Name] = pkgFunc{body: fd.Body, path: path}
		}
	}
	return idx
}

func checkFile(fset *token.FileSet, path string, denylist map[string]bool, pkgFuncs map[string]pkgFunc) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		// 寻找 Transition{...} 字面量
		composite, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typeName := exprToString(composite.Type)
		if !strings.HasSuffix(typeName, "Transition") {
			return true
		}

		// 找 Effects 字段
		for _, elt := range composite.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			keyIdent, ok := kv.Key.(*ast.Ident)
			if !ok || keyIdent.Name != "Effects" {
				continue
			}

			// funcLit 为 Effects 闭包
			funcLit, ok := kv.Value.(*ast.FuncLit)
			if !ok {
				continue
			}

			closureCount++
			checkEffectsClosure(fset, path, funcLit, denylist, pkgFuncs)
		}
		return true
	})
}

func checkEffectsClosure(fset *token.FileSet, path string, fn *ast.FuncLit, denylist map[string]bool, pkgFuncs map[string]pkgFunc) {
	if fn.Body == nil {
		return
	}
	// via 为空表示直接命中；非空表示经由某个同包函数间接命中（一层展开）。
	scanBody(fset, path, fn.Body, denylist, pkgFuncs, "", true)
}

// scanBody 扫描一个函数体。expand 为 true 时，对体内调用的同包函数再下探一层。
func scanBody(
	fset *token.FileSet, path string, body *ast.BlockStmt,
	denylist map[string]bool, pkgFuncs map[string]pkgFunc, via string, expand bool,
) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fnStr := exprToString(call.Fun)
		// SafeGo 内的 IO 是异步非阻塞，不占 FSM 锁，放行且不再下探。
		if strings.Contains(fnStr, "SafeGo") {
			return false
		}

		name := ""
		switch t := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = t.Sel.Name
		case *ast.Ident:
			name = t.Name
		}
		if name == "" {
			return true
		}

		if denylist[name] {
			pos := fset.Position(call.Pos())
			if via == "" {
				fmt.Printf("%s:%d: Effects 闭包内包含黑名单 IO 方法调用 %q（违反 inv_FSM_B1，锁内禁止同步 IO）\n",
					path, pos.Line, name)
			} else {
				fmt.Printf("%s:%d: Effects 闭包经 %s() 间接调用黑名单 IO 方法 %q（违反 inv_FSM_B1，锁内禁止同步 IO）\n",
					path, pos.Line, via, name)
			}
			errCount++
			return true
		}

		// 一层调用图展开：闭包直接调用的同包函数，其函数体也要扫。
		if expand {
			if target, ok := pkgFuncs[name]; ok && target.body != body {
				scanBody(fset, target.path, target.body, denylist, pkgFuncs, name, false)
			}
		}
		return true
	})
}

func exprToString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		return exprToString(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}
