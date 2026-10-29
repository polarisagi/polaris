package lint_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLintOutboxEntry(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 排除测试和依赖目录
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.Contains(path, "testdata") || strings.Contains(path, "vendor") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr // ignore parse errors
		}

		ast.Inspect(f, func(n ast.Node) bool {
			// Find protocol.OutboxEntry{ TargetEngine: "literal" }
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}

			// 检查是否为 OutboxEntry
			isOutboxEntry := false
			if sel, ok := cl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "OutboxEntry" {
				isOutboxEntry = true
			} else if ident, ok := cl.Type.(*ast.Ident); ok && ident.Name == "OutboxEntry" {
				isOutboxEntry = true
			}

			if isOutboxEntry {
				for _, elt := range cl.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if keyIdent, ok := kv.Key.(*ast.Ident); ok && keyIdent.Name == "TargetEngine" {
							if _, isLiteral := kv.Value.(*ast.BasicLit); isLiteral {
								pos := fset.Position(kv.Pos())
								relPath, _ := filepath.Rel(root, pos.Filename)
								violations = append(violations, relPath+":"+pos.String()+": TargetEngine should not be a literal string (use protocol.Topic* constants)")
							}
						}
					}
				}
			}
			return true
		})
		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("Found %d violations for bare string TargetEngine in OutboxEntry:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func TestLintTypeAssertions(t *testing.T) {
	root := repoRoot(t)
	var violations []string

	// 匹配 \.\(\*mem(store|retrieval)\. 和 \.\(\*learning\.Engine\) 的断言
	regexAssertion := regexp.MustCompile(`\.\(\*(memstore|memretrieval|learning)\.`)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.Contains(path, "testdata") || strings.Contains(path, "vendor") {
			return nil
		}

		relPath, _ := filepath.Rel(root, path)

		// 允许在 cmd/polaris, internal/memory, internal/learning 内部使用
		if strings.HasPrefix(filepath.ToSlash(relPath), "cmd/polaris/") ||
			strings.HasPrefix(filepath.ToSlash(relPath), "internal/memory/") ||
			strings.HasPrefix(filepath.ToSlash(relPath), "internal/learning/") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr
		}

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if regexAssertion.MatchString(line) {
				violations = append(violations, relPath+":"+string(rune(i+1))+": illegal concrete type assertion outside of cmd/polaris")
			}
		}

		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("Found %d violations for illegal concrete type assertions:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// responseWriterWrapperTypes 收集全仓（internal/ + pkg/）内匿名嵌入 http.ResponseWriter
// 的结构体类型名，供 TestResponseWriterWrapperImplementsUnwrap 复用。
func responseWriterWrapperTypes(t *testing.T, root string) map[string]token.Position {
	t.Helper()
	types := map[string]token.Position{}
	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				if len(field.Names) != 0 {
					continue // 非匿名嵌入
				}
				sel, ok := field.Type.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" && sel.Sel.Name == "ResponseWriter" {
					types[ts.Name.Name] = fset.Position(ts.Pos())
				}
			}
			return true
		})
	})
	return types
}

// TestResponseWriterWrapperImplementsUnwrap (ADR-0094 决策七) 校验全仓所有匿名嵌入
// http.ResponseWriter 的包装类型都实现了 Unwrap() http.ResponseWriter。缺失时
// Go 1.20+ http.ResponseController（SetWriteDeadline 等）无法穿透包装层定位底层
// http.Flusher/http.Hijacker，静默失效而不报错——反例见修复前的 LoggingResponseWriter。
func TestResponseWriterWrapperImplementsUnwrap(t *testing.T) {
	root := repoRoot(t)
	wrapperTypes := responseWriterWrapperTypes(t, root)
	hasUnwrap := map[string]bool{}

	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Name.Name != "Unwrap" {
				return true
			}
			recvType := fn.Recv.List[0].Type
			if star, ok := recvType.(*ast.StarExpr); ok {
				recvType = star.X
			}
			if ident, ok := recvType.(*ast.Ident); ok {
				hasUnwrap[ident.Name] = true
			}
			return true
		})
	})

	for name, pos := range wrapperTypes {
		if !hasUnwrap[name] {
			t.Errorf("ResponseWriterWrapperImplementsUnwrap VIOLATED: %s (%s) 匿名嵌入 http.ResponseWriter 但未实现 Unwrap() http.ResponseWriter", name, pos)
		}
	}
}

// TestSlogHandlerWrapperPreservesSelf (ADR-0094 决策七) 校验实现 slog.Handler 的
// 包装类型（持有一个 slog.Handler 字段）的 WithAttrs/WithGroup 方法，不得直接
// return <底层字段>.WithAttrs(...)/WithGroup(...) 剥离自身包装——那会让所有经
// logger.With(...)/WithGroup(...) 派生的子 logger 绕过本类型承载的额外能力
// （反例见修复前的 LogStore：剥离后子 logger 的日志不再进 SSE 广播）。
func TestSlogHandlerWrapperPreservesSelf(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		// 找出本文件中持有 slog.Handler 字段的结构体类型名。
		handlerFieldTypes := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				sel, ok := field.Type.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "slog" && sel.Sel.Name == "Handler" {
					handlerFieldTypes[ts.Name.Name] = true
				}
			}
			return true
		})
		if len(handlerFieldTypes) == 0 {
			return
		}

		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				return true
			}
			if fn.Name.Name != "WithAttrs" && fn.Name.Name != "WithGroup" {
				return true
			}
			recvType := fn.Recv.List[0].Type
			if star, ok := recvType.(*ast.StarExpr); ok {
				recvType = star.X
			}
			ident, ok := recvType.(*ast.Ident)
			if !ok || !handlerFieldTypes[ident.Name] {
				return true
			}
			// 方法体唯一一条语句是 `return <field>.WithAttrs(...)`/`WithGroup(...)`
			// 时判定为剥离自身包装。
			if len(fn.Body.List) != 1 {
				return true
			}
			ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				return true
			}
			call, ok := ret.Results[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			callSel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || callSel.Sel.Name != fn.Name.Name {
				return true
			}
			// 底层是字段选择表达式（如 s.wrapped）而非构造自身类型的字面量/工厂函数。
			if _, isFieldSelect := callSel.X.(*ast.SelectorExpr); isFieldSelect {
				pos := fset.Position(fn.Pos())
				violations = append(violations, violation{
					relPath: relPath, line: pos.Line,
					detail: fn.Name.Name + " 直接返回底层 slog.Handler 字段的 " + fn.Name.Name + "(...)，剥离了 " + ident.Name + " 自身包装",
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("SlogHandlerWrapperPreservesSelf VIOLATED: %s", v)
	}
}
