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

func main() {
	fset := token.NewFileSet()
	var goFiles []string
	err := filepath.Walk("internal", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Walk failed: %v\n", err)
		os.Exit(2)
	}

	baselineMap := make(map[string]bool)
	baselineBytes, err := os.ReadFile("local_playground/reports/apperr-semantics-baseline.md")
	if err == nil {
		lines := strings.Split(string(baselineBytes), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "internal/") {
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
