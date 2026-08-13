//go:build ignore

// nolint_unused_lint 检查 //nolint:unused 抑制失效与存量接线候选 (F-11)。
//
// 规则：
//   - 若标注 `//nolint:unused` 的符号在全仓有生产调用方 → FAIL (抑制已失效，必须删除)
//   - 若无生产调用方 → PASS，但输出到 local_playground/reports/nolint-unused-inventory.md 保持追踪
//
// 使用：
//
//	go run tools/nolint_unused_lint.go
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

type NolintItem struct {
	FilePath   string
	Line       int
	SymbolName string
	HasCallers bool
}

func main() {
	roots := []string{"internal", "cmd", "pkg"}
	fset := token.NewFileSet()
	var items []NolintItem

	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkFile(fset, path, &items)
			return nil
		})
	}

	// 检查每一个 nolint 符号在全仓的生产调用
	var activeInventory []string
	for _, item := range items {
		callCount := countProductionCallers(roots, item.SymbolName, item.FilePath)
		if callCount > 0 {
			fmt.Printf("%s:%d: 符号 %q 已有 %d 处生产调用，但仍挂着 //nolint:unused 抑制（违反 F-11 失效抑制清理规则）\n",
				item.FilePath, item.Line, item.SymbolName, callCount)
			errCount++
		} else {
			entry := fmt.Sprintf("%s:%d: %s (无生产调用方，接线候选)", item.FilePath, item.Line, item.SymbolName)
			activeInventory = append(activeInventory, entry)
		}
	}

	// 输出存量清单
	invPath := "local_playground/reports/nolint-unused-inventory.md"
	writeInventory(invPath, activeInventory)

	fmt.Printf("nolint_unused_lint: scanned %d //nolint:unused item(s) (inventory: %s)\n", len(items), invPath)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "nolint_unused_lint: FAIL — %d stale suppression(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("nolint_unused_lint: PASS")
}

func checkFile(fset *token.FileSet, path string, items *[]NolintItem) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	// 解析带有注释的 AST
	node, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return
	}

	// 寻找 `//nolint:unused`
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.Contains(line, "//nolint:unused") || strings.Contains(line, "// nolint:unused") {
			lineNo := i + 1
			// 在对应行附近寻找定义的 FuncDecl 或 GenDecl
			symName := findSymbolAtLine(fset, node, lineNo)
			if symName != "" {
				*items = append(*items, NolintItem{
					FilePath:   path,
					Line:       lineNo,
					SymbolName: symName,
				})
			}
		}
	}
}

func findSymbolAtLine(fset *token.FileSet, node *ast.File, line int) string {
	var sym string
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		pos := fset.Position(n.Pos())
		// 允许 2 行偏移
		if pos.Line >= line && pos.Line <= line+2 {
			switch fn := n.(type) {
			case *ast.FuncDecl:
				sym = fn.Name.Name
				return false
			case *ast.TypeSpec:
				sym = fn.Name.Name
				return false
			case *ast.ValueSpec:
				if len(fn.Names) > 0 {
					sym = fn.Names[0].Name
					return false
				}
			}
		}
		return true
	})
	return sym
}

// countProductionCallers 统计 symName 在生产代码中被引用的次数（不含其自身声明）。
//
// 2026-08-12 改为 AST 判定。原实现是逐行字符串匹配，并用
// `!strings.Contains(l, "func ")` 跳过声明行——这个启发式同时吃掉了所有写在单行
// 函数体里的真实调用（`func caller() int { return target() }`），属于会让失效抑制
// 继续隐身的漏报方向。经 make lint-selftest 负向验证暴露：注入一个「有调用方却挂着
// //nolint:unused」的样例，规则报 PASS。
//
// AST 版按标识符判定：统计所有名为 symName 的 *ast.Ident，再扣掉声明节点自身的
// 名字节点，剩下的即引用。同名局部变量会被计入（保守方向——宁可少报「抑制失效」，
// 不可漏报），这一取舍写在此处以免后来者误以为是缺陷。
func countProductionCallers(roots []string, symName, declPath string) int {
	if symName == "" {
		return 0
	}
	count := 0
	fset := token.NewFileSet()
	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			node, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(node, func(n ast.Node) bool {
				switch d := n.(type) {
				case *ast.FuncDecl:
					// 声明自身的名字节点不算引用；函数体照常继续遍历
					// （同一函数内递归调用仍会被计入，符合"有人用"的语义）。
					if d.Name != nil && d.Name.Name == symName {
						if d.Recv != nil {
							ast.Inspect(d.Recv, countIdent(symName, &count))
						}
						ast.Inspect(d.Type, countIdent(symName, &count))
						if d.Body != nil {
							ast.Inspect(d.Body, countIdent(symName, &count))
						}
						return false
					}
				case *ast.TypeSpec:
					if d.Name != nil && d.Name.Name == symName {
						ast.Inspect(d.Type, countIdent(symName, &count))
						return false
					}
				case *ast.ValueSpec:
					for _, nm := range d.Names {
						if nm.Name == symName {
							// 只跳过名字本身，初始化表达式仍算引用。
							for _, v := range d.Values {
								ast.Inspect(v, countIdent(symName, &count))
							}
							return false
						}
					}
				case *ast.Ident:
					if d.Name == symName {
						count++
					}
				}
				return true
			})
			return nil
		})
	}
	return count
}

// countIdent 返回一个只做「同名标识符计数」的 ast.Inspect 回调。
func countIdent(symName string, count *int) func(ast.Node) bool {
	return func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == symName {
			*count++
		}
		return true
	}
}

func writeInventory(path string, items []string) {
	os.MkdirAll(filepath.Dir(path), 0755) //nolint:errcheck
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintln(f, "# //nolint:unused 存量未接线候选清单（F-11 追踪）")
	fmt.Fprintln(f, "# 本文件由 tools/nolint_unused_lint.go 自动生成，记录被 unused 遮盖但无生产调用方的符号。")
	fmt.Fprintln(f)
	for _, item := range items {
		fmt.Fprintln(f, "-", item)
	}
}
