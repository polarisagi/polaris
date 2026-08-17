//go:build ignore

// bounded_cache_check 断言 bufio.Scanner 的单行上限来自 internal/config 阀值，
// 而不是写死在调用点（L-10）。
//
// 2026-08-17 重写判据。原实现只把第二个实参匹配为 *ast.BasicLit，于是：
//
//	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)  ← BinaryExpr，看不见
//	scanner.Buffer(buf, 102400)                            ← BasicLit，才报
//
// 而全仓 8 个调用点里有 7 个写成前者，唯一写对的那个用的是 config 阀值——也就是说
// 本规则自诞生起从未报过一次红，绿灯完全来自判据的盲区。更糟的是 lint-selftest
// 给它的注入样例恰好是 102400（唯一能被抓的形态），负向验证因此也常绿，
// 「已验证」变成了假象。同轮把用例改成 1024*1024，让自测覆盖真实写法。
//
// 现判据：
//  1. 接收者必须确认是 bufio.NewScanner(...) 的返回值（消除对任意 .Buffer(a,b) 的误报）；
//  2. 第二个实参若为**编译期字面量常量表达式**（字面量本身，或全由字面量经算术/括号/
//     一元运算构成）即违规；命名常量、config 阀值、变量均放行。
//
// 棘轮：存量记在 tools/baselines/bounded-cache-baseline.md，只禁增量。
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

// scanRoots 与其余规则保持同一份清单。2026-08-17 从单 "internal" 扩到三根——
// cmd/polaris/cli.go 里就有一处 Buffer 调用，此前不在任何规则的视野内（ADR-0089）。
var scanRoots = []string{"internal", "cmd", "pkg"}

const baselinePath = "tools/baselines/bounded-cache-baseline.md"

func main() {
	baseline := loadBaseline()
	fset := token.NewFileSet()

	scanned := 0
	hasError := false
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			scanners := scannerIdents(f)

			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Buffer" || len(call.Args) != 2 {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok || !scanners[recv.Name] {
					// 接收者不是本文件内 bufio.NewScanner 的返回值 → 不是本规则的判据面。
					return true
				}
				scanned++
				if !isLiteralConstExpr(call.Args[1]) {
					return true
				}
				pos := fset.Position(call.Pos())
				line := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
				if baseline[line] {
					return true
				}
				fmt.Fprintf(os.Stderr, "%s: bufio.Scanner.Buffer 的上限写成字面量常量表达式，"+
					"必须引用 internal/config 阀值（违反 L-10）\n", line)
				hasError = true
				return true
			})
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Walk %s failed: %v\n", root, err)
			os.Exit(2)
		}
	}

	// 锚点自毁断言：本规则的判据面是「bufio.Scanner 的 Buffer 调用」。一个都找不到，
	// 说明扫描根、接收者识别或调用形态之一已经变了，而不是"仓库很干净"。
	// 让规则的消失表现为红灯，是 ADR-0091 那四种门控失真形态的解药。
	if scanned == 0 {
		fmt.Fprintf(os.Stderr, "bounded-cache-check: FAIL — 全仓找不到任何 bufio.Scanner.Buffer 调用点，"+
			"判据面已失效，请同步本规则而非让它继续静默通过\n")
		os.Exit(2)
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Printf("bounded-cache-check ok（%d 个 Buffer 调用点，%d 条存量在 baseline）\n", scanned, len(baseline))
}

// isLiteralConstExpr 判定表达式是否为编译期字面量常量：字面量本身，或全部由字面量
// 经算术/括号/一元运算构成（1024*1024、10*1024*1024、-1 等）。
// 命名常量与选择器（config.CurrentThresholds()....）一律返回 false。
func isLiteralConstExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.INT
	case *ast.ParenExpr:
		return isLiteralConstExpr(x.X)
	case *ast.UnaryExpr:
		return isLiteralConstExpr(x.X)
	case *ast.BinaryExpr:
		return isLiteralConstExpr(x.X) && isLiteralConstExpr(x.Y)
	}
	return false
}

// scannerIdents 收集本文件内由 bufio.NewScanner(...) 赋值的标识符名。
// 只看同文件内的直接赋值：跨文件/跨函数传递的 Scanner 不在判据面内，宁可漏报也
// 不误报——L-10 的价值在"新写的调用点别写死"，不在把存量翻个底朝天。
func scannerIdents(f *ast.File) map[string]bool {
	out := map[string]bool{}
	isNewScanner := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewScanner" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "bufio"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range x.Rhs {
				if i < len(x.Lhs) && isNewScanner(rhs) {
					if id, ok := x.Lhs[i].(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range x.Values {
				if i < len(x.Names) && isNewScanner(v) {
					out[x.Names[i].Name] = true
				}
			}
		}
		return true
	})
	return out
}

// loadBaseline 读取棘轮基线：每行首个 `path:line` token 生效，其余为说明文字。
func loadBaseline() map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		tok := strings.TrimSuffix(fields[0], ":")
		for _, root := range scanRoots {
			if strings.HasPrefix(tok, root+"/") && strings.Contains(tok, ":") {
				out[tok] = true
			}
		}
	}
	return out
}
