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
// 把真正的漂移淹掉。首词是**另一个不相干标识符**的情形（真改名后未跟进）需要全仓
// 符号表才能零误报判定，暂不纳入——见本文件 git 历史里手工修的 2 处。
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
