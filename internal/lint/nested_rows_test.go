package lint_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── inv_NoNestedRowsQuery ───────────────────────────────────────────────────

var (
	rowsLoopRe = regexp.MustCompile(`for\s+\w*[Rr]ows\.Next\(\)`)
	dbCallRe   = regexp.MustCompile(`\.(QueryContext|QueryRowContext|ExecContext|Query|Exec)\(`)
)

// Test_inv_NoNestedRowsQuery 禁止在 rows.Next() 循环体内再发数据库查询。
//
// 外层 *sql.Rows 未关闭时会一直占住一条连接，内层查询要另取一条：连接池被占满后，
// 内层拿不到连接、外层又在等内层完成，整个池死锁。这不是理论风险——
// internal/store 的 readDB 池 MaxOpenConns=4，第 4 个并发请求即触发；测试用的
// :memory: 库只有一条连接，有任意一行数据就必然卡死。
//
// 2026-08-09 全仓补齐 :memory: 单连接约束后，plugin.HandleListPlugins 当场卡死暴露
// 出这个模式（外层查 plugins、循环体内逐行查 mcp_servers）。已改为两段式读取：
// 先把外层结果读进内存并关闭 rows，再一次性拉取关联表在内存归组——顺带消掉 N+1。
//
// 正确写法：外层先 drain 成 slice 并 Close，再做后续查询；或一次 JOIN 取回。
func Test_inv_NoNestedRowsQuery(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	for _, sub := range []string{"internal", "pkg", "cmd"} {
		err := filepath.Walk(filepath.Join(root, sub), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil //nolint:nilerr
			}
			relPath, _ := filepath.Rel(root, path)
			lines := strings.Split(string(src), "\n")
			for i, l := range lines {
				if !rowsLoopRe.MatchString(l) {
					continue
				}
				depth := 0
				for j := i; j < len(lines); j++ {
					depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
					if j > i && depth <= 0 {
						break
					}
					if j > i && dbCallRe.MatchString(lines[j]) {
						violations = append(violations, violation{
							relPath: relPath,
							line:    j + 1,
							detail:  "rows.Next() 循环体内嵌套数据库查询 — 外层 Rows 占住连接不放，池满即死锁；须先 drain 外层再查",
						})
						break
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk failed: %v", err)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_NoNestedRowsQuery VIOLATED: %s", v)
	}
}
