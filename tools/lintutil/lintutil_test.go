package lintutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestIsLiteralConstExpr 覆盖 L-10 判据的历史盲区：只认 BasicLit 会漏掉 1024*1024
// 这类 BinaryExpr，而仓库里的实际写法恰恰全是后者。
func TestIsLiteralConstExpr(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"102400", true},
		{"1024 * 1024", true},
		{"10 * 1024 * 1024", true},
		{"(4 * 1024) * 2", true},
		{"-1", true},
		{"1.5", true},
		{"maxScanBytes", false},
		{"config.CurrentThresholds().M7Tool.MCPStdioMaxScanBytes", false},
		{"cfg.Max * 1024", false},
		{`"a string"`, false},
	}
	for _, c := range cases {
		e, err := parser.ParseExpr(c.src)
		if err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		if got := IsLiteralConstExpr(e); got != c.want {
			t.Errorf("IsLiteralConstExpr(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestExprText(t *testing.T) {
	cases := map[string]string{
		"a":            "a",
		"a.b":          "a.b",
		"m.policyGate": "m.policyGate",
		"a.b.c.d":      "a.b.c.d",
		"(a.b)":        "a.b",
		"f()":          "",
		"a[0].b":       "",
	}
	for src, want := range cases {
		e, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if got := ExprText(e); got != want {
			t.Errorf("ExprText(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestHasNilGuard(t *testing.T) {
	src := `package p
func withGuard(m *M) error {
	if m.policyGate == nil {
		return errSentinel
	}
	return m.policyGate.Review()
}
func withoutGuard(m *M) error {
	if m.other == nil {
		return errSentinel
	}
	return m.policyGate.Review()
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	FuncDecls(File{Path: "p.go", AST: f, Fset: fset}, func(fd *ast.FuncDecl) {
		got[fd.Name.Name] = HasNilGuard(fd.Body, "m.policyGate")
	})
	if !got["withGuard"] {
		t.Error("withGuard 有 m.policyGate == nil 判定，应识别为已守卫")
	}
	if got["withoutGuard"] {
		t.Error("withoutGuard 守的是 m.other，不应被当作 m.policyGate 的 nil 判定")
	}
}

// TestParseBaseline 覆盖统一抑制表格式：纯清单、Markdown 列表、带理由、注释、散文行。
func TestParseBaseline(t *testing.T) {
	text := `# 这是注释
## 标题也应被忽略

internal/a/b.go:12
- internal/c/d.go:34 这条的理由写在后面
  internal/e/f.go:56: 冒号结尾也算
rust/substrate/src/x.rs:7
这是一行散文，不以扫描根开头，必须被忽略
cmd/polaris/cli.go:402 本地 stdin`

	got := parseBaseline(text)
	want := []string{
		"internal/a/b.go:12",
		"internal/c/d.go:34",
		"internal/e/f.go:56",
		"rust/substrate/src/x.rs:7",
		"cmd/polaris/cli.go:402",
	}
	if got.Len() != len(want) {
		t.Fatalf("解析出 %d 条，期望 %d 条：%v", got.Len(), len(want), got)
	}
	for _, k := range want {
		if !got.Has(k) {
			t.Errorf("缺少条目 %q", k)
		}
	}
	if got.Has("这是一行散文，不以扫描根开头，必须被忽略") {
		t.Error("散文行不应被当成条目")
	}
}

// TestKeySetNilSafe 保证 fail-closed 规则（baseline 为 nil）不会 panic。
func TestKeySetNilSafe(t *testing.T) {
	var s KeySet
	if s.Has("anything") {
		t.Error("nil KeySet 不应抑制任何条目")
	}
	if s.Len() != 0 {
		t.Error("nil KeySet 长度应为 0")
	}
}

// TestWalkOptionsSkip 锁定扫描面语义：默认跳过测试文件，IncludeTests 打开后不跳。
func TestWalkOptionsSkip(t *testing.T) {
	def := WalkOptions{}
	if !def.skip("internal/a/b_test.go") {
		t.Error("默认应跳过 _test.go")
	}
	if def.skip("internal/a/b.go") {
		t.Error("默认不应跳过普通 .go")
	}
	if !def.skip("internal/a/b.md") {
		t.Error("非 .go 一律跳过")
	}

	withTests := WalkOptions{IncludeTests: true}
	if withTests.skip("internal/a/b_test.go") {
		t.Error("IncludeTests 打开后不应跳过 _test.go")
	}

	excl := WalkOptions{ExcludeContains: []string{"internal/security/network/"}}
	if !excl.skip("internal/security/network/safe_dialer.go") {
		t.Error("ExcludeContains 未生效")
	}
	if excl.skip("internal/security/taint/x.go") {
		t.Error("ExcludeContains 误伤了不相关路径")
	}
}

// TestWalkOptionsRootsDefault 保证「忘记写 Roots」得到的是全仓三根而不是空扫描。
// 这条锁的是 ADR-0089 那类失效：规则停在一个根上、或干脆一个文件都没扫却打印 PASS。
func TestWalkOptionsRootsDefault(t *testing.T) {
	if got := (WalkOptions{}).roots(); len(got) != 3 {
		t.Fatalf("默认扫描根应为三根，实得 %v", got)
	}
	if got := (WalkOptions{Roots: []string{"internal/store"}}).roots(); len(got) != 1 {
		t.Fatalf("显式收窄未生效：%v", got)
	}
}
