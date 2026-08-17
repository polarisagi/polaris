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

// scanRoots 是本仓所有 Go 源码根。与 must_check_error / rows_err / todo / nolint 等
// 规则保持同一份清单——扫描根不一致本身就是一种静默漏检（ADR-0089）。
var scanRoots = []string{"internal", "cmd", "pkg"}

// isScannedPath 判定一行 baseline 文本是否以某个扫描根开头（即它是一条路径记录，
// 而非 baseline 文件里的散文说明）。
func isScannedPath(line string) bool {
	for _, root := range scanRoots {
		if strings.HasPrefix(line, root+"/") {
			return true
		}
	}
	return false
}

func main() {
	fset := token.NewFileSet()
	// 扫描根：2026-08-17 从单 "internal" 扩到三根。原因见 ADR-0089——「规则停在一个
	// 扫描根上」是本仓复发过的失效形态，且 cmd/ 与 pkg/ 里有 230 处 apperr.New/Wrap
	// 从未被本规则看过。扩根前已实测：cmd/ 与 pkg/ 上 0 新增命中，属零成本对齐而非还债。
	var goFiles []string
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
				goFiles = append(goFiles, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Walk %s failed: %v\n", root, err)
			os.Exit(2)
		}
	}

	baselineMap := make(map[string]bool)
	baselineBytes, err := os.ReadFile("tools/baselines/apperr-semantics-baseline.md")
	if err == nil {
		lines := strings.Split(string(baselineBytes), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if isScannedPath(line) {
				baselineMap[line] = true
			}
		}
	}

	hasError := false
	for _, path := range goFiles {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			xIdent, ok := sel.X.(*ast.Ident)
			if !ok || xIdent.Name != "apperr" {
				return true
			}

			if sel.Sel.Name != "New" && sel.Sel.Name != "Wrap" {
				return true
			}

			if len(call.Args) < 2 {
				return true
			}

			codeArg := call.Args[0]
			msgArg := call.Args[1]
			if sel.Sel.Name == "Wrap" {
				if len(call.Args) < 3 {
					return true
				}
				codeArg = call.Args[0]
				msgArg = call.Args[1]
			}

			codeSel, ok := codeArg.(*ast.SelectorExpr)
			var actualCode string
			if ok {
				actualCode = codeSel.Sel.Name
			} else {
				actualCode = fmt.Sprintf("%v", codeArg)
			}

			msgLit, ok := msgArg.(*ast.BasicLit)
			if !ok || msgLit.Kind != token.STRING {
				return true
			}

			msg := strings.Trim(msgLit.Value, `"`)
			msgLower := strings.ToLower(msg)

			expectedCode := ""
			triggerKeyword := ""

			if strings.Contains(msgLower, "rate limit") || strings.Contains(msgLower, "quota") || strings.Contains(msgLower, "exhausted") || strings.Contains(msgLower, "too many") {
				expectedCode = "CodeResourceExhausted"
				if strings.Contains(msgLower, "rate limit") {
					triggerKeyword = "rate limit"
				}
				if strings.Contains(msgLower, "quota") {
					triggerKeyword = "quota"
				}
				if strings.Contains(msgLower, "exhausted") {
					triggerKeyword = "exhausted"
				}
				if strings.Contains(msgLower, "too many") {
					triggerKeyword = "too many"
				}
			} else if strings.Contains(msgLower, "not found") {
				expectedCode = "CodeNotFound"
				triggerKeyword = "not found"
			} else if strings.Contains(msgLower, "forbidden") || strings.Contains(msgLower, "denied") {
				expectedCode = "CodeForbidden"
				if strings.Contains(msgLower, "forbidden") {
					triggerKeyword = "forbidden"
				}
				if strings.Contains(msgLower, "denied") {
					triggerKeyword = "denied"
				}
			}

			if expectedCode != "" && actualCode != expectedCode {
				pos := fset.Position(call.Pos())
				errLine := fmt.Sprintf("%s:%d: apperr message 含 %q 应使用 %s 错误码，实际是 %s", pos.Filename, pos.Line, triggerKeyword, expectedCode, actualCode)

				if baselineMap[errLine] {
					// skipped by baseline
				} else {
					fmt.Fprintf(os.Stderr, "%s（违反 L-11 R2.5）\n", errLine)
					hasError = true
				}
			}

			return true
		})
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Println("apperr-semantics-check ok")
}
