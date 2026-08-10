package lint_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintTaintedStringAccess(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.Contains(path, "testdata") || strings.Contains(path, "vendor") {
			return nil //nolint:nilerr
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}

		relPath, _ := filepath.Rel(root, path)

		// 检查是否有直接调用 Value() 方法，且其可能是 TaintedString / SafeString
		ast.Inspect(f, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			selExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			if selExpr.Sel.Name == "Value" {
				// We found a .Value() call. Check if it's allowed.
				if strings.HasSuffix(filepath.ToSlash(relPath), "internal/security/taint/taint_sanitizer.go") {
					return true
				}

				// ADR-0047: SanitizeByDeterministicTransform is allowed
				inAllowedFunc := false
				ast.Inspect(f, func(parent ast.Node) bool {
					if fn, ok := parent.(*ast.FuncDecl); ok {
						if fn.Name.Name == "SanitizeByDeterministicTransform" && fn.Pos() <= selExpr.Pos() && selExpr.Pos() <= fn.End() {
							inAllowedFunc = true
						}
					}
					return true
				})

				if inAllowedFunc {
					return true
				}

				hasTaintImport := false
				for _, imp := range f.Imports {
					if strings.Contains(imp.Path.Value, "internal/security/taint") {
						hasTaintImport = true
						break
					}
				}

				if hasTaintImport {
					pos := fset.Position(selExpr.Pos())
					violations = append(violations, relPath+":"+pos.String()+": direct access to .Value() is prohibited outside taint_sanitizer.go. (Exception: SanitizeByDeterministicTransform as per ADR-0047)")
				}
			}
			return true
		})
		return nil //nolint:nilerr
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("Found %d violations for direct TaintedString/SafeString access:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func TestLintSecurityExportsCalled(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	exportedFuncs := make(map[string]string)

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)
		if !strings.HasPrefix(relSlash, "internal/security/taint") &&
			!strings.HasPrefix(relSlash, "internal/security/policy") &&
			!strings.HasPrefix(relSlash, "internal/security/guard") {
			return nil //nolint:nilerr
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() {
				if fn.Recv == nil {
					exportedFuncs[fn.Name.Name] = relSlash
				}
			}
		}
		return nil //nolint:nilerr
	})

	called := make(map[string]bool)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if _, exists := exportedFuncs[sel.Sel.Name]; exists {
					called[sel.Sel.Name] = true
				}
			}
			if ident, ok := n.(*ast.Ident); ok {
				if _, exists := exportedFuncs[ident.Name]; exists {
					called[ident.Name] = true
				}
			}
			return true
		})
		return nil //nolint:nilerr
	})

	var violations []string
	for fn, file := range exportedFuncs {
		if !called[fn] {
			violations = append(violations, file+": exported function "+fn+" has no non-test callers")
		}
	}

	if len(violations) > 0 {
		t.Logf("Found %d uncalled security functions:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// TestFailClosedSafetyVerdict (ADR-0094 决策一) 检查 security 与 agents 包中返回 AuditResult / 风险判定结果的 parse 函数，
// 禁止在 error / parse 失败分支返回 RiskLevel: "none" 或隐式放行。
func TestFailClosedSafetyVerdict(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	targetPaths := []string{
		filepath.Join(root, "internal", "swarm", "agents"),
		filepath.Join(root, "internal", "security"),
	}

	for _, dir := range targetPaths {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil //nolint:nilerr
			}

			rel, _ := filepath.Rel(root, path)

			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}

				// 检查 parseAuditResult 或类似 parse 判定函数
				if strings.HasPrefix(fn.Name.Name, "parse") || strings.HasSuffix(fn.Name.Name, "Result") {
					ast.Inspect(fn.Body, func(bn ast.Node) bool {
						ret, ok := bn.(*ast.ReturnStmt)
						if !ok {
							return true
						}
						// 检查是否在 return 语句中构造并返回了 RiskLevel: "none"
						for _, expr := range ret.Results {
							if cl, ok := expr.(*ast.UnaryExpr); ok {
								expr = cl.X
							}
							if cl, ok := expr.(*ast.CompositeLit); ok {
								for _, elt := range cl.Elts {
									if kv, ok := elt.(*ast.KeyValueExpr); ok {
										if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "RiskLevel" {
											if val, ok := kv.Value.(*ast.BasicLit); ok && strings.Contains(strings.ToLower(val.Value), "none") {
												pos := fset.Position(ret.Pos())
												violations = append(violations, rel+":"+pos.String()+": fail-closed violation: "+fn.Name.Name+" returns RiskLevel: \"none\" on fallback path")
											}
										}
									}
								}
							}
						}
						return true
					})
				}
				return true
			})
			return nil
		})
	}

	if len(violations) > 0 {
		t.Errorf("Found %d fail-closed safety verdict violations:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// TestNoRawStringIntoStructuredSink (ADR-0094 决策六，原名 TestStructuredSinksAntiInjection)
// 校验 internal/store/audit/ 下 SQL Exec/Query 系列调用不得用字符串 "+" 拼接
// 构造查询语句本身。
//
// 范围维持原状（仅 audit/）而不放大到全仓：2026-08-10 复核曾尝试放大到
// internal/ + pkg/ 全仓，结果对 11 处"+"拼接触发误报——这些拼接的操作数
// 全部是编译期 const 列名列表（core_memory.go coreMemorySelectCols）或由
// len(args) 驱动的占位符生成（sqlite_blackboard_ops.go "?" + strings.Repeat(...)），
// 不携带任何运行时不可信数据，语法层面的"+"拼接检测无法区分这两类操作数
// 与真正的注入面。收窄回原范围，避免规则本身背离 CLAUDE.md
// "先验证规则本身再决定是否收窄" 的教训。真正需要全仓覆盖的新检查项见
// TestFTS5MatchArgsUseQuoteHelper。
func TestNoRawStringIntoStructuredSink(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	targetDir := filepath.Join(root, "internal", "store", "audit")
	_ = filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil //nolint:nilerr
		}
		rel, _ := filepath.Rel(root, path)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "QueryContext" || sel.Sel.Name == "ExecContext" {
					if len(call.Args) >= 2 {
						queryArg := call.Args[1]
						if binary, ok := queryArg.(*ast.BinaryExpr); ok && binary.Op == token.ADD {
							pos := fset.Position(call.Pos())
							violations = append(violations, rel+":"+pos.String()+": string concatenation in SQL query argument (anti-injection rule)")
						}
					}
				}
			}
			return true
		})
		return nil
	})

	if len(violations) > 0 {
		t.Errorf("Found %d structured sink injection violations:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// TestFTS5MatchArgsUseQuoteHelper (ADR-0094 决策六) 全仓（internal/ + pkg/）扫描：
// 含 "MATCH ?" 的 SQLite FTS5 查询语句所在调用，其绑定参数必须经
// util.QuoteFTS5 / util.QuoteFTS5Query 转义——反例见 2026-08-10 修复前的
// graph_traverser.go（entityName 直拼）/ retriever.go / rag_retrieval.go /
// repo_chat.go（用户查询词直拼）。与 SQL 字符串拼接检查不同，FTS5 语法字符
// （"、*、:、AND/OR/NOT）出现在合法用户输入中是常态而非攻击特征，无法用
// "是否是 const/是否含运行时变量" 来降噪，因此该检查天然不会像上面的
// concat 检查那样对良性拼接产生误报，可以安全地覆盖全仓。
func TestFTS5MatchArgsUseQuoteHelper(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "QueryContext", "ExecContext", "QueryRowContext":
			default:
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, "MATCH ?") {
				return true
			}
			hasQuoteHelper := false
			for _, arg := range call.Args[2:] {
				argCall, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				argSel, ok := argCall.Fun.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := argSel.X.(*ast.Ident); ok && pkg.Name == "util" &&
					(argSel.Sel.Name == "QuoteFTS5" || argSel.Sel.Name == "QuoteFTS5Query") {
					hasQuoteHelper = true
				}
			}
			if !hasQuoteHelper {
				pos := fset.Position(call.Pos())
				violations = append(violations, violation{
					relPath: relPath, line: pos.Line,
					detail: `FTS5 "MATCH ?" 查询的绑定参数未经 util.QuoteFTS5/QuoteFTS5Query 转义`,
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("FTS5MatchArgsUseQuoteHelper VIOLATED: %s", v)
	}
}
