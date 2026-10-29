package lint_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ─── inv_MetricNaming（ADR-0113 决策五 / Constitution R2.6）──────────────────

var (
	// OTel instrument 名：polaris.<段>.<段>…，小写 + 数字 + 下划线，点号至少分出两段。
	otelMetricNameRe = regexp.MustCompile(`^polaris\.[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	// Prometheus 原生（promauto / prometheus.*Opts）指标名：polaris_ 前缀。
	promMetricNameRe = regexp.MustCompile(`^polaris_[a-z0-9_]+$`)

	otelInstrumentMethods = map[string]bool{
		"Int64Counter": true, "Float64Counter": true,
		"Int64Histogram": true, "Float64Histogram": true,
		"Int64Gauge": true, "Float64Gauge": true,
		"Int64UpDownCounter": true, "Float64UpDownCounter": true,
		"Int64ObservableGauge": true, "Float64ObservableGauge": true,
		"Int64ObservableCounter": true, "Float64ObservableCounter": true,
		"Int64ObservableUpDownCounter": true, "Float64ObservableUpDownCounter": true,
	}
	promOptsTypes = map[string]bool{
		"CounterOpts": true, "GaugeOpts": true, "HistogramOpts": true,
		"SummaryOpts": true, "Opts": true,
	}
)

// Test_inv_MetricNaming 扫描仓库 internal、cmd、pkg 三个源码根下的非测试 .go 文件：
//   - 形如 X.Int64Counter("name", …) 等 OTel instrument 创建调用，首参字面量须匹配
//     ^polaris\.[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$；
//   - prometheus.CounterOpts/GaugeOpts/HistogramOpts/SummaryOpts/Opts 复合字面量里的
//     Name: 字面量须匹配 ^polaris_[a-z0-9_]+$。
//
// 判据边界（刻意不覆盖，宁漏不误报）：名字不是字符串字面量（变量、常量、拼接、
// 表驱动 c.name）一律跳过；Prometheus Opts 带 Namespace/Subsystem 时 Name 只是尾段，
// 此时按字面量 Name 校验同样会报，需改为完整 polaris_ 名；手写 /metrics 文本输出
// （fmt.Fprintf "# TYPE ..."）不在扫描范围。_test.go 与 testdata 目录不扫。
func Test_inv_MetricNaming(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	for _, sub := range []string{"internal", "cmd", "pkg"} {
		err := filepath.Walk(filepath.Join(root, sub), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil //nolint:nilerr
			}
			if info.IsDir() {
				if info.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil //nolint:nilerr
			}
			s := string(src)
			if !strings.Contains(s, "Int64") && !strings.Contains(s, "Float64") && !strings.Contains(s, "Opts") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, src, 0)
			if perr != nil {
				return nil //nolint:nilerr
			}
			relPath, _ := filepath.Rel(root, path)
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok || !otelInstrumentMethods[sel.Sel.Name] || len(x.Args) == 0 {
						return true
					}
					if name, ok := stringLit(x.Args[0]); ok && !otelMetricNameRe.MatchString(name) {
						violations = append(violations, violation{
							relPath: relPath,
							line:    fset.Position(x.Args[0].Pos()).Line,
							detail:  `OTel instrument "` + name + `" 不符合 polaris.<subsystem>.<name>（点号分段，至少两段）`,
						})
					}
				case *ast.CompositeLit:
					sel, ok := x.Type.(*ast.SelectorExpr)
					if !ok || !promOptsTypes[sel.Sel.Name] {
						return true
					}
					if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "prometheus" {
						return true
					}
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Name" {
							continue
						}
						if name, ok := stringLit(kv.Value); ok && !promMetricNameRe.MatchString(name) {
							violations = append(violations, violation{
								relPath: relPath,
								line:    fset.Position(kv.Value.Pos()).Line,
								detail:  `Prometheus 原生指标 "` + name + `" 须以 polaris_ 开头且仅含小写字母/数字/下划线`,
							})
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk failed: %v", err)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_MetricNaming VIOLATED: %s", v)
	}
}

// stringLit 返回表达式的字符串字面量值；非字面量（变量/常量/拼接）返回 ok=false。
func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return v, true
}
