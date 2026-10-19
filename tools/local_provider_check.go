//go:build ignore

// local_provider_check 确保 local_only 判定口径全局唯一（R2-07）：
//
//	[L-21] 禁止在 llm.IsLocalProvider 之外出现针对 provider 类型或名称做本地判断的字符串比较。
//
// 唯一权威判定函数为 llm.IsLocalProvider(providerRow)，依据 DB 中的权威字段判定。
// 禁止在 registry、factory、list_models、resolveModelRoute、spawn 等处使用散落的
// typ == "ollama"、strings.Contains(lower, "local") 等启发式比较。
package main

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

var bannedLiterals = map[string]bool{
	"ollama":      true,
	"llama_local": true,
}

func main() {
	r := lintutil.NewReporter("local-provider-check", nil)
	lintutil.Walk(r, lintutil.WalkOptions{
		ExcludeContains: []string{"vendor/"},
	}, func(f lintutil.File) {
		rel := filepath.ToSlash(f.Path)
		if isExempt(rel) {
			return
		}

		r.Anchor()
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BinaryExpr:
				checkBinaryExpr(r, f, node)
			case *ast.CallExpr:
				checkCallExpr(r, f, node)
			}
			return true
		})
	})

	r.RequireAnchors(10, "local-provider-check: 扫描面命中 < 10，可能路径配置错误")
	r.Done()
}

func isExempt(rel string) bool {
	if strings.HasSuffix(rel, "_test.go") {
		return true
	}
	if strings.HasPrefix(rel, "tools/") {
		return true
	}
	if rel == "internal/llm/provider_local.go" {
		return true
	}
	// 排除与 LLM provider 本地性判断无关的模块
	exemptPrefixes := []string{
		"cmd/polaris/boot_embedding.go",
		"cmd/polaris/cli.go",
		"internal/config/config.go",
		"internal/llm/ollamamgr/",
		"internal/security/network/local_only.go",
		"internal/gateway/server/sysadmin/system_update.go",
		"internal/gateway/server/server_handlers_hitl.go",
		"internal/gateway/server/provider/catalog.go",
		"internal/gateway/server/provider/providers_probe.go",
		"internal/llm/modelcatalog/prober.go",
		"internal/llm/adapter/embedding.go",
		"internal/automation/cost_report.go",
	}
	for _, p := range exemptPrefixes {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

func checkBinaryExpr(r *lintutil.Reporter, f lintutil.File, expr *ast.BinaryExpr) {
	if expr.Op != token.EQL && expr.Op != token.NEQ {
		return
	}
	if isBannedStringLit(expr.X) || isBannedStringLit(expr.Y) {
		pos := f.Fset.Position(expr.Pos())
		r.Violation(
			filepath.ToSlash(pos.Filename)+":"+strconv.Itoa(pos.Line),
			"[L-21] 禁止在 llm.IsLocalProvider 之外出现针对 provider 类型或名称做本地判断的字符串比较；必须调用 llm.IsLocalProvider 唯一权威判定（R2-07）",
		)
	}
}

func checkCallExpr(r *lintutil.Reporter, f lintutil.File, call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "strings" {
		return
	}
	fnName := sel.Sel.Name
	if fnName != "Contains" && fnName != "HasPrefix" && fnName != "HasSuffix" && fnName != "EqualFold" {
		return
	}

	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING {
			unquoted, _ := strconv.Unquote(lit.Value)
			lower := strings.ToLower(unquoted)
			if lower == "local" || lower == "ollama" || lower == "llama" {
				pos := f.Fset.Position(call.Pos())
				r.Violation(
					filepath.ToSlash(pos.Filename)+":"+strconv.Itoa(pos.Line),
					"[L-21] 禁止使用 strings."+fnName+" 匹配 "+lit.Value+" 进行本地 Provider 启发式判断；必须调用 llm.IsLocalProvider（R2-07）",
				)
			}
		}
	}
}

func isBannedStringLit(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	return bannedLiterals[unquoted]
}
