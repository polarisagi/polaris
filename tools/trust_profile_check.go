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

// trustProfileIdentNames 档位常量标识符（无论裸写还是通过 package policy 引用）。
var trustProfileIdentNames = map[string]bool{
	"ProfileStandard": true,
	"ProfileFull":     true,
	"ProfileHardened": true,
}

// checkBinaryExpr 检查 `x == "full"` / `pol.Profile == policy.ProfileHardened` 形式的档位比较。
func checkBinaryExpr(r *lintutil.Reporter, f lintutil.File, expr *ast.BinaryExpr) {
	if expr.Op != token.EQL && expr.Op != token.NEQ {
		return
	}

	// 1. 任一侧为档位值（字面量如 "hardened"，或常量如 policy.ProfileHardened）
	if isTrustProfileValue(expr.X) || isTrustProfileValue(expr.Y) {
		pos := f.Fset.Position(expr.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+itoa(pos.Line),
			"[L-20] 禁止在 ProfilePolicy 外直接比较 TrustProfile 档位值；"+
				"应通过 policy.Policy 的字段访问安全行为（ADR-0115 WP1 / R3-08）",
		)
		return
	}

	// 2. 任一侧为 .Profile 字段，且另一侧非 nil（捕获对 .Profile 字段的 == / != 比较，R3-08）
	if (isProfileField(expr.X) && !isNil(expr.Y)) || (isProfileField(expr.Y) && !isNil(expr.X)) {
		pos := f.Fset.Position(expr.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+itoa(pos.Line),
			"[L-20] 禁止在 ProfilePolicy 外直接对 .Profile 字段进行 ==/!= 比较；"+
				"应通过 policy.Policy 的字段访问安全行为（ADR-0115 WP1 / R3-08）",
		)
	}
}

// checkSwitchStmt 检查 switch 语句中以 .Profile 字段为条件或 case 值含档位常量的分支。
func checkSwitchStmt(r *lintutil.Reporter, f lintutil.File, sw *ast.SwitchStmt) {
	// 1. switch tag 为 .Profile 字段（如 switch pol.Profile { ... }，R3-08）
	if sw.Tag != nil && isProfileField(sw.Tag) {
		pos := f.Fset.Position(sw.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+itoa(pos.Line),
			"[L-20] 禁止在 ProfilePolicy 外对 .Profile 字段做 switch 分支；"+
				"应通过 policy.Policy 字段访问（ADR-0115 WP1 / R3-08）",
		)
		return
	}

	// 2. case 子句中出现档位常量或字面量
	hitConstCount := 0
	hitLitCount := 0
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range cc.List {
			if isTrustProfileIdentOrConst(expr) {
				hitConstCount++
			}
			if isTrustProfileLiteral(expr) {
				hitLitCount++
			}
		}
	}
	if hitConstCount >= 1 || hitLitCount >= 2 {
		pos := f.Fset.Position(sw.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+itoa(pos.Line),
			"[L-20] 禁止在 ProfilePolicy 外用 switch/case 分支档位值；"+
				"应通过 policy.Policy 字段访问（ADR-0115 WP1 / R3-08）",
		)
	}
}

// isTrustProfileIdentOrConst 判断表达式是否为档位常量标识符（如 policy.ProfileHardened 或 ProfileHardened）。
func isTrustProfileIdentOrConst(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return trustProfileIdentNames[e.Sel.Name]
	case *ast.Ident:
		return trustProfileIdentNames[e.Name]
	}
	return false
}

// isTrustProfileValue 判断表达式是否为档位字面量或常量。
func isTrustProfileValue(expr ast.Expr) bool {
	return isTrustProfileLiteral(expr) || isTrustProfileIdentOrConst(expr)
}

// isProfileField 判断表达式是否访问了 .Profile 字段（如 pol.Profile）。
func isProfileField(expr ast.Expr) bool {
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == "Profile"
	}
	return false
}

// isNil 判断表达式是否为 nil。
func isNil(expr ast.Expr) bool {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name == "nil"
	}
	return false
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
