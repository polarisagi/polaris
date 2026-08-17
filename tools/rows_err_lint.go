//go:build ignore

// rows_err_lint 扫描所有 SQL for X.Next() 循环后是否包含对应的 X.Err() 检查（F-7）。
//
// 特征：支持动态提取 iterator 变量名 (rows / mrows / pr / r 等)，防止硬编码 rows 导致漏检 (B-8 案件)。
// 棘轮机制：存量历史漏检记录在 tools/baselines/rows-err-baseline.md，
//
//	本门控阻断任何**新增**的无 rows.Err() 校验 SQL 循环。
//
// 使用：
//
//	go run tools/rows_err_lint.go
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
var loopCount int
var baselineCount int

func main() {
	baselinePath := "tools/baselines/rows-err-baseline.md"
	baselineMap, _ := loadBaseline(baselinePath)

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
			checkFile(fset, path, baselineMap)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "rows_err_lint: walk %s: %v\n", root, err)
			os.Exit(2)
		}
	}

	fmt.Printf("rows_err_lint: scanned %d SQL rows.Next() loop(s) (baseline: %d stock item(s))\n", loopCount, baselineCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "rows_err_lint: FAIL — %d NEW violation(s) not in baseline\n", errCount)
		os.Exit(1)
	}
	fmt.Println("rows_err_lint: PASS")
}

func loadBaseline(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	bm := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bm[line] = true
	}
	return bm, scanner.Err()
}

func checkFile(fset *token.FileSet, path string, baseline map[string]bool) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		checkFunc(fset, path, fn, baseline)
		return true
	})
}

func checkFunc(fset *token.FileSet, path string, fn *ast.FuncDecl, baseline map[string]bool) {
	type nextLoop struct {
		varName string
		pos     token.Pos
	}
	var loops []nextLoop
	errCheckedVars := make(map[string]bool)

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if forStmt, ok := n.(*ast.ForStmt); ok && forStmt.Cond != nil {
			if call, ok := forStmt.Cond.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Next" {
					if ident, ok := sel.X.(*ast.Ident); ok {
						loops = append(loops, nextLoop{varName: ident.Name, pos: forStmt.Pos()})
						loopCount++
					}
				}
			}
		}

		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Err" {
				if ident, ok := sel.X.(*ast.Ident); ok {
					errCheckedVars[ident.Name] = true
				}
			}
		}
		return true
	})

	for _, l := range loops {
		if !errCheckedVars[l.varName] {
			pos := fset.Position(l.pos)
			key := fmt.Sprintf("%s:%d:%s", path, pos.Line, l.varName)
			if baseline[key] {
				baselineCount++
				continue // 存量在棘轮基线中，跳过
			}
			fmt.Printf("%s:%d: 新增的 for %s.Next() 循环后缺少 %s.Err() 检查（违反 F-7 数据库迭代安全约束与棘轮规则）\n",
				path, pos.Line, l.varName, l.varName)
			errCount++
		}
	}
}
