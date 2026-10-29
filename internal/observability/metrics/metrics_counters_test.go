package metrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// 每个 Global*Total atomic.Int64 都必须出现在 simpleCounters() 中（GR-1.2-004 回归门控）：
// 新增计数器而忘记暴露 = 能算不上报（HE-1），这里在 go test 阶段直接报红。
func TestSimpleCounters_CoverAllGlobalTotals(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			vs, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			sel, ok := vs.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Int64" {
				return true
			}
			for _, id := range vs.Names {
				if strings.HasPrefix(id.Name, "Global") && strings.HasSuffix(id.Name, "Total") {
					declared[id.Name] = true
				}
			}
			return true
		})
	}

	f, err := parser.ParseFile(fset, "metrics_counters.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	exposed := map[string]bool{}
	ast.Inspect(f, func(node ast.Node) bool {
		if u, ok := node.(*ast.UnaryExpr); ok && u.Op == token.AND {
			if id, ok := u.X.(*ast.Ident); ok {
				exposed[id.Name] = true
			}
		}
		return true
	})

	if len(declared) == 0 {
		t.Fatal("no Global*Total counters found; parser selector broken")
	}
	for name := range declared {
		if !exposed[name] {
			t.Errorf("%s 未在 simpleCounters() 中暴露到 /metrics", name)
		}
	}
	if got := len(simpleCounters()); got != len(exposed) {
		t.Errorf("simpleCounters() len=%d, AST refs=%d", got, len(exposed))
	}
}
