package lint_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── inv_MemDBSingleConn ─────────────────────────────────────────────────────

var (
	memOpenRe = regexp.MustCompile(`(\w+), (?:err|_) :?= sql\.Open\([^)]*:memory:[^)]*\)`)
	memSetRe  = `%s.SetMaxOpenConns(`
)

// Test_inv_MemDBSingleConn 要求 sql.Open(..., ":memory:") 之后限制连接池为单连接。
//
// sqlite 的 :memory: 在不带 cache=shared 时，**每条连接都是一个彼此独立的空库**。
// database/sql 的池在没有空闲连接可复用时会新开一条——单 goroutine 顺序查询碰不到，
// 一旦出现并发、或前一个 *sql.Rows 未关闭就发起第二次查询，第二条连接就落到空库上，
// 表现为"表不存在 / 查不到刚写入的行"这类只在 CI 上偶发的 flaky。
//
// internal/store.OpenSQLite 早就对 :memory: 做了特判（复用 writer 连接），但散落在
// 各包测试里的 102 处裸 sql.Open 没有。2026-07-29 修过其中一处，同款隐患一直挂着；
// 2026-08-09 全量补齐并加本规则封口。
//
// 判定只认两种正确写法：SetMaxOpenConns(1)，或 DSN 里带 cache=shared。
func Test_inv_MemDBSingleConn(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	for _, sub := range []string{"internal", "pkg", "cmd"} {
		err := filepath.Walk(filepath.Join(root, sub), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil //nolint:nilerr
			}
			src, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(src), ":memory:") {
				return nil //nolint:nilerr
			}
			relPath, _ := filepath.Rel(root, path)
			lines := strings.Split(string(src), "\n")
			for i, l := range lines {
				m := memOpenRe.FindStringSubmatch(l)
				if m == nil || strings.Contains(l, "cache=shared") {
					continue
				}
				hi := i + 14
				if hi > len(lines) {
					hi = len(lines)
				}
				if strings.Contains(strings.Join(lines[i:hi], "\n"), strings.ReplaceAll(memSetRe, "%s", m[1])) {
					continue
				}
				violations = append(violations, violation{
					relPath: relPath,
					line:    i + 1,
					detail:  `sql.Open(":memory:") 未限制连接池 — 须紧随 ` + m[1] + `.SetMaxOpenConns(1)，否则第二条连接是另一个空库`,
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk failed: %v", err)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_MemDBSingleConn VIOLATED: %s", v)
	}
}
