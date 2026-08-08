package lint_test

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// ─── inv_DocCommentNameCase ──────────────────────────────────────────────────

var docFirstWordRe = regexp.MustCompile(`^([A-Za-z_]\w*)\b`)

// Test_inv_DocCommentNameCase 禁止文档注释首词与函数名**仅大小写不同**。
//
// 这是导出化/非导出化重构后最典型的注释漂移：函数从 handleListPlugins 改名为
// HandleListPlugins，注释首词留在原地。2026-08-08 一次全仓扫描查出 37 处，其中 33 处
// 是这个形态——同一个包里往往整片如此（sysadmin/ 一个目录就 15 处），说明它是批量
// 重构的副产物而非个别疏忽，靠人工复审必然复发。
//
// 判定刻意只收「大小写不同」这一种，不强制 Go 官方的「doc comment 必须以标识符开头」：
// 本仓注释是中文，大量函数的注释以中文起头是既定风格，全量强制会产出数百条噪声，
// 把真正的漂移淹掉。首词是**另一个函数名**的情形由 Test_inv_DocCommentOrphan 负责。
func Test_inv_DocCommentNameCase(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil || len(fn.Doc.List) == 0 {
				continue
			}
			text := strings.TrimSpace(strings.TrimPrefix(fn.Doc.List[0].Text, "//"))
			m := docFirstWordRe.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			word, name := m[1], fn.Name.Name
			if word == name || !strings.EqualFold(word, name) {
				continue
			}
			violations = append(violations, violation{
				relPath: relPath,
				line:    fset.Position(fn.Doc.List[0].Pos()).Line,
				detail:  "文档注释首词 " + word + " 与函数名 " + name + " 仅大小写不同 — 重构改名后注释未跟进",
			})
		}
	})

	for _, v := range violations {
		t.Errorf("inv_DocCommentNameCase VIOLATED: %s", v)
	}
}

// ─── inv_DocCommentOrphan ────────────────────────────────────────────────────

// Test_inv_DocCommentOrphan 禁止文档注释首词是**另一个已声明的函数名**。
//
// 这是函数被移动/改名/拆分后最典型的残留：上一个函数的 doc 块留在原地，被紧随其后的
// 函数继承，godoc 会把描述挂到错误的函数上。2026-08-09 全仓扫描查出 6 处，例如
// learning.Engine 的三环主循环文档（旧名 Run）落在了 handleTaskCompleteEvent 头上，
// 而真正的 Start 反倒一行文档都没有。
//
// 只比对**函数名**、不比对类型名：中文注释以类型名起头（"Agent 的执行循环…"）是
// 正常语序，纳入会误报；以另一个函数名起头则几乎必然是复制粘贴或改名残留。
func Test_inv_DocCommentOrphan(t *testing.T) {
	root := repoRoot(t)

	funcs, types := map[string]bool{}, map[string]bool{}
	walkRepoGoFiles(t, root, nil, func(_ *token.FileSet, f *ast.File, _ string) {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				funcs[d.Name.Name] = true
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					if ts, ok := sp.(*ast.TypeSpec); ok {
						types[ts.Name.Name] = true
					}
				}
			}
		}
	})

	var violations []violation
	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil || len(fn.Doc.List) == 0 {
				continue
			}
			text := strings.TrimSpace(strings.TrimPrefix(fn.Doc.List[0].Text, "//"))
			m := docFirstWordRe.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			word, name := m[1], fn.Name.Name
			if word == name || strings.EqualFold(word, name) {
				continue // 大小写变体交给 inv_DocCommentNameCase
			}
			if !funcs[word] || types[word] {
				continue
			}
			violations = append(violations, violation{
				relPath: relPath,
				line:    fset.Position(fn.Doc.List[0].Pos()).Line,
				detail:  "文档注释首词是另一个函数名 " + word + "，但挂在 " + name + " 上 — 函数移动/改名后 doc 块残留",
			})
		}
	})

	for _, v := range violations {
		t.Errorf("inv_DocCommentOrphan VIOLATED: %s", v)
	}
}
