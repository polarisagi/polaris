// Package lint_test 静态扫描不变量 CI 测试。
// 使用 go/ast 精确检测调用点，字符串字面量不触发误报。
package lint_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot 返回仓库根目录（此文件在 internal/lint/，向上两级）。
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	// 防卫性校验：存在 go.mod 才是真正的根目录
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		t.Fatalf("repoRoot: go.mod not found under %s", abs)
	}
	return abs
}

// violation 描述一处违规调用点。
type violation struct {
	relPath string
	line    int
	detail  string
}

func (v violation) String() string {
	return fmt.Sprintf("%s:%d: %s", v.relPath, v.line, v.detail)
}

// walkPkgGoFiles 遍历 root/pkg/ 下所有非测试 .go 文件，返回解析后的 AST。
// 跳过 exemptRel 中列出的相对路径（相对于 root）。
func walkPkgGoFiles(t *testing.T, root string, exemptRel map[string]bool,
	fn func(fset *token.FileSet, f *ast.File, relPath string)) {

	t.Helper()
	pkgDir := filepath.Join(root, "pkg")
	err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if exemptRel[rel] {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil //nolint:nilerr // 跳过解析失败的文件（生成代码等）
		}
		fn(fset, f, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walkPkgGoFiles: %v", err)
	}
}

// ─── inv_M1_01 ────────────────────────────────────────────────────────────────

// Test_inv_M1_01_NoRawHTTPCalls 验证 pkg/ 中不存在裸 HTTP 调用。
// inv_M1_01: 所有 LLM 调用经 Provider Router，禁止裸 http.Get/Post/Head 调用和
//
//	直接引用 http.DefaultClient。
//
// 被扫描的禁止模式：
//   - http.Get(...)  / http.Post(...)  / http.Head(...) — 包级 HTTP 便捷函数
//   - http.DefaultClient — 全局客户端直接引用（绕过 SafeDialer SSRF 防护）
func Test_inv_M1_01_NoRawHTTPCalls(t *testing.T) {
	root := repoRoot(t)

	// 豁免说明：
	//   safe_dialer.go      — SafeDialer 实现，合法持有 http.Transport/DefaultTransport
	//   inference/http_client.go — 包级 var 初始为 DefaultClient，由 SetDefaultHTTPClient 在
	//                              进程启动时替换（仅单元测试场景保留回退，架构已文档化）
	exempt := map[string]bool{
		filepath.Join("pkg", "substrate", "safe_dialer.go"):              true,
		filepath.Join("pkg", "substrate", "inference", "http_client.go"): true,
	}

	// 禁止的 http 包成员名（调用或引用均算违规）
	forbiddenHTTPSelectors := map[string]bool{
		"Get":           true,
		"Post":          true,
		"Head":          true,
		"DefaultClient": true,
	}

	var violations []violation
	walkPkgGoFiles(t, root, exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkgIdent.Name != "http" {
				return true
			}
			if forbiddenHTTPSelectors[sel.Sel.Name] {
				pos := fset.Position(sel.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  fmt.Sprintf("http.%s — 裸 HTTP 调用/引用，须改用 substrate.NewSafeHTTPClient", sel.Sel.Name),
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_M1_01 VIOLATED: %s", v)
	}
}

// ─── inv_M11_05 / inv_M7_06 ──────────────────────────────────────────────────

// Test_inv_M11_05_NoRawNetDial 验证 pkg/ 中无裸 net.Dial / net.DialContext 调用。
// inv_M11_05: 所有出站连接经 SafeDialer.DialContext 五阶段 SSRF 防护——HTTP/3 QUIC 禁用。
// inv_M7_06:  所有出站连接强制经 M11 SafeDialer.DialContext——禁止裸 net.Dial/grpc.Dial。
//
// 扫描范围: pkg/ 下所有非测试 .go 文件中的 CallExpr（字符串字面量不触发）。
// 精确匹配规则: CallExpr{Fun: SelectorExpr{X: Ident("net"), Sel: "Dial"/"DialContext"}}
//
//	或          CallExpr{Fun: SelectorExpr{X: Ident("grpc"), Sel: "Dial"/"NewClient"}}
func Test_inv_M11_05_NoRawNetDial(t *testing.T) {
	root := repoRoot(t)

	// 豁免说明：
	//   safe_dialer.go — SafeDialer 实现内部使用 net.Dialer{}.DialContext（struct method，非包函数）
	//                    以及 net.SplitHostPort / net.ParseIP 等 DNS 工具函数，均不在禁止列表内。
	//                    此豁免保留以防未来维护者添加 net.Dial 包级调用。
	exempt := map[string]bool{
		filepath.Join("pkg", "substrate", "safe_dialer.go"): true,
	}

	// pkg="net", sel in {"Dial","DialContext"} 或 pkg="grpc", sel in {"Dial","NewClient"}
	type forbidden struct{ pkg, sel string }
	forbiddenDialCalls := []forbidden{
		{"net", "Dial"},
		{"net", "DialContext"},
		{"grpc", "Dial"},
		{"grpc", "NewClient"},
	}

	var violations []violation
	walkPkgGoFiles(t, root, exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			// 仅检查 CallExpr 中的 Fun，避免变量名 net.Dialer 之类误报
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fb := range forbiddenDialCalls {
				if pkgIdent.Name == fb.pkg && sel.Sel.Name == fb.sel {
					pos := fset.Position(call.Pos())
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  fmt.Sprintf("%s.%s(...) — 裸网络拨号，须改用 substrate.SafeDialer.DialContext", fb.pkg, fb.sel),
					})
				}
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_M11_05/inv_M7_06 VIOLATED: %s", v)
	}
}

// ─── 辅助测试：验证扫描逻辑本身的正确性 ─────────────────────────────────────

// Test_inv_LintScanner_DoesNotFlagStringLiterals 验证扫描器对字符串字面量中的
// "net.Dial" "http.Get" 等模式不产生误报。
// 背景：pkg/cognition/skill_pipeline.go 的 StaticAnalyzer.Analyze 将这些字符串作为
// 被扫描目标（字符串比较），不是实际调用——AST 方案应正确区分。
func Test_inv_LintScanner_DoesNotFlagStringLiterals(t *testing.T) {
	// 构造一个包含字符串字面量"net.Dial"的合成文件，解析后确认无 CallExpr 违规
	src := `package test

import "strings"

func check(code string) bool {
	return strings.Contains(code, "net.Dial") ||
		strings.Contains(code, "http.Get") ||
		strings.Contains(code, "grpc.Dial")
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}

	callCount := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name == "net" && (sel.Sel.Name == "Dial" || sel.Sel.Name == "DialContext") {
			callCount++
			t.Errorf("false positive: net.Dial string literal flagged as call at line %d", fset.Position(call.Pos()).Line)
		}
		if pkg.Name == "http" && (sel.Sel.Name == "Get" || sel.Sel.Name == "DefaultClient") {
			callCount++
			t.Errorf("false positive: http.Get string literal flagged as call at line %d", fset.Position(call.Pos()).Line)
		}
		return true
	})

	if callCount > 0 {
		t.Errorf("scanner incorrectly flagged %d string literal(s) as call violations", callCount)
	}
}
