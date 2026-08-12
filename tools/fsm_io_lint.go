//go:build ignore

// fsm_io_lint 扫描 internal/agent/fsm/ 中 Transition.Effects 闭包内持锁同步 IO 调用（F-4）。
//
// 限制说明：只展开一层 AST 节点。深层 IO 依赖 code review，限制已在工具头部声明。
// 外部名单：tools/fsm-io-denylist.txt
//
// 负向验证：临时在 fsm/transitions.go 某一 Effects 闭包内加入 MatchIntent 调用 → 本门控报红。
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

	err = filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checkFile(fset, path, denylist)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsm_io_lint: walk %s: %v\n", targetDir, err)
		os.Exit(2)
	}

	fmt.Printf("fsm_io_lint: scanned %d Effects closure(s)\n", closureCount)
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

func checkFile(fset *token.FileSet, path string, denylist map[string]bool) {
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
			checkEffectsClosure(fset, path, funcLit, denylist)
		}
		return true
	})
}

func checkEffectsClosure(fset *token.FileSet, path string, fn *ast.FuncLit, denylist map[string]bool) {
	if fn.Body == nil {
		return
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		// 检查 SafeGo 包裹（在 SafeGo 内的 IO 属于异步非阻塞，允许）
		if call, ok := n.(*ast.CallExpr); ok {
			fnStr := exprToString(call.Fun)
			if strings.Contains(fnStr, "SafeGo") {
				return false // 不再递归检查 SafeGo 内部
			}

			// 检查选择器调用 Obj.Method(...)
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				methodName := sel.Sel.Name
				if denylist[methodName] {
					pos := fset.Position(call.Pos())
					fmt.Printf("%s:%d: Effects 闭包内包含黑名单 IO 方法调用 %q（违反 inv_FSM_B1，锁内禁止同步 IO）\n",
						path, pos.Line, methodName)
					errCount++
				}
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
