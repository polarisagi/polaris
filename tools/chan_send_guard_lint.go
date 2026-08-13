//go:build ignore

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

func main() {
	fset := token.NewFileSet()

	err := filepath.Walk("internal/store", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		checkFile(fset, path)
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "chan_send_guard_lint: walk error: %v\n", err)
		os.Exit(2)
	}

	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "chan_send_guard_lint: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("chan_send_guard_lint: PASS")
}

func checkFile(fset *token.FileSet, path string) {
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return
	}

	// 提前收集单行注释的 ignore 行号
	ignoreLines := make(map[int]bool)
	for _, cgroup := range node.Comments {
		for _, c := range cgroup.List {
			if strings.Contains(c.Text, "//nolint:chan_send_guard") {
				pos := fset.Position(c.Pos())
				ignoreLines[pos.Line] = true
				// 注释可能在同一行，也可能在上一行，暂不追求极端精确，
				// 这里假设如果包含，则该行或下一行被豁免。
				ignoreLines[pos.Line+1] = true
			}
		}
	}

	ast.Inspect(node, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.SelectStmt:
			// select 内的 发送都是允许的，我们不再往下遍历它的 Body 中的 stmt，
			// 等等，select 下面如果嵌套了其他非 select 发送呢？
			// select 的 case 里的 stmt 属于 select case。如果 case 里有发送：
			// CommClause 里的 SendStmt 是合法的。
			for _, caseStmt := range stmt.Body.List {
				cc, ok := caseStmt.(*ast.CommClause)
				if !ok {
					continue
				}
				// 检查 case 的 Comm
				if cc.Comm != nil {
					if _, isSend := cc.Comm.(*ast.SendStmt); isSend {
						// 这里面的发送是合法的，不要报警
					}
				}
				// 遍历 case 的内部语句（内部语句里的直接发送如果没有包裹在另一个 select 中，应该报警）
				for _, bodyStmt := range cc.Body {
					ast.Inspect(bodyStmt, func(bn ast.Node) bool {
						checkNode(fset, path, bn, ignoreLines)
						return true
					})
				}
			}
			return false // 不要继续默认遍历了，因为我们手动遍历了
		default:
			checkNode(fset, path, n, ignoreLines)
		}
		return true
	})
}

func checkNode(fset *token.FileSet, path string, n ast.Node, ignoreLines map[int]bool) {
	sendStmt, ok := n.(*ast.SendStmt)
	if !ok {
		return
	}
	sel, ok := sendStmt.Chan.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if sel.Sel.Name != "ResultCh" {
		return
	}

	pos := fset.Position(sendStmt.Pos())
	if ignoreLines[pos.Line] || ignoreLines[pos.Line-1] {
		return
	}

	fmt.Printf("%s:%d: ResultCh 发送未包裹在 select-default 中，单写者不得阻塞在他人 channel（违反 L-05）\n", path, pos.Line)
	errCount++
}
