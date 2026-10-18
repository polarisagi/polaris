//go:build ignore

// trust_profile_check 确保信任档位闸门遵循 ADR-0115 WP1 决策二：
//
//	[L-20] ProfilePolicy 外禁止直接比较 TrustProfile 字符串（profile == "full" 等同类形式）。
//
// 所有安全行为差异必须通过 Policy struct 字段访问，由 ProfilePolicy 这一唯一权威点映射。
// 这样添加新档位时只需修改 ProfilePolicy 一处，不会出现散落的 if profile == "xxx" 遗漏分支。
//
// 例外（不报错）：
//   - package policy 自身（ProfilePolicy 的实现就在里面）
//   - 测试文件（*_test.go）
//   - 本 lint 工具自身（tools/trust_profile_check.go）
//
// 检测形态（覆盖 AST 级常见写法）：
//  1. BinaryExpr：`profile == "full"` / `pol.Profile == "hardened"` / `TrustProfile("...") == x`
//  2. SwitchStmt：switch body 内有 ≥2 个档位字面量的 case 子句
//  3. 内嵌在类型转换里的字面量比较：`string(profile) == "full"`
//
// 锚点设计：每个扫到的 Go 文件（非 policy 包、非 test）都计一次 Anchor，
// 确保规则失效（扫描面缩为 0）时 RequireAnchors 触发而非绿灯。
package main

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

// trustProfileLiterals 档位字符串字面量集合，用于判断 case / BinaryExpr 是否在比较档位值。
var trustProfileLiterals = map[string]bool{
	"full": true, "standard": true, "hardened": true,
}

func main() {
	r := lintutil.NewReporter("trust-profile-check", nil) // fail-closed：存量违规不接受
	lintutil.Walk(r, lintutil.WalkOptions{
		// 三个扫描根：全量扫描（收窄会漏掉 cmd/ 里的调用）
		ExcludeContains: []string{"vendor/"},
	}, func(f lintutil.File) {
		rel := filepath.ToSlash(f.Path)
		if isExempt(rel) {
			return
		}
		// 每个非豁免文件计一次锚点（判据面存在性证明）
		r.Anchor()
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BinaryExpr:
				checkBinaryExpr(r, f, node)
			case *ast.SwitchStmt:
				checkSwitchStmt(r, f, node)
			}
			return true
		})
	})
	// 要求至少扫到 10 个文件（仓库不可能真的删光所有 Go 源码）
	r.RequireAnchors(10,
		"trust-profile-check：扫描面命中 <10，可能 WalkOptions 配置错误或仓库结构变化")
	r.Done()
}

// isExempt 判断路径是否豁免：policy 包自身、测试文件、工具目录。
func isExempt(rel string) bool {
	if strings.Contains(rel, "internal/security/policy/") {
		return true
	}
	if strings.HasSuffix(rel, "_test.go") {
		return true
	}
	if strings.HasPrefix(rel, "tools/") {
		return true
	}
	return false
}

// checkBinaryExpr 检查 `x == "full"` / `x != "hardened"` 形式的档位比较。
func checkBinaryExpr(r *lintutil.Reporter, f lintutil.File, expr *ast.BinaryExpr) {
	if expr.Op != token.EQL && expr.Op != token.NEQ {
		return
	}
	if isTrustProfileLiteral(expr.X) || isTrustProfileLiteral(expr.Y) {
		pos := f.Fset.Position(expr.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+itoa(pos.Line),
			"[L-20] 禁止在 ProfilePolicy 外直接比较 TrustProfile 字符串字面量；"+
				"应通过 policy.Policy 的字段访问安全行为（ADR-0115 WP1）",
		)
	}
}

// checkSwitchStmt 检查 switch 语句中 case 值含多个档位字面量的情形。
// ≥2 个档位字面量出现在同一 switch body → 很可能是在重复 ProfilePolicy 的映射逻辑。
func checkSwitchStmt(r *lintutil.Reporter, f lintutil.File, sw *ast.SwitchStmt) {
	hitCount := 0
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range cc.List {
			if isTrustProfileLiteral(expr) {
				hitCount++
			}
		}
	}
	if hitCount >= 2 {
		pos := f.Fset.Position(sw.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+itoa(pos.Line),
			"[L-20] 禁止在 ProfilePolicy 外用 switch/case 分支多个档位字面量；"+
				"应通过 policy.Policy 字段访问（ADR-0115 WP1）",
		)
	}
}

// isTrustProfileLiteral 判断表达式是否为档位字符串字面量（含类型转换形式）。
func isTrustProfileLiteral(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			val := strings.Trim(e.Value, `"`)
			return trustProfileLiterals[val]
		}
	case *ast.CallExpr:
		// TrustProfile("full") / string(profile) 等单参数类型转换
		if len(e.Args) == 1 {
			return isTrustProfileLiteral(e.Args[0])
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
