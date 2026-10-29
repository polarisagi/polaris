package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// TestGuardSchemaDowngrade 守护 ADR-0095 决策三：库的 schema 版本高于二进制内嵌
// 版本时必须拒绝启动。
//
// 直接测 guardSchemaDowngrade 而非跑一次真实 OpenSQLite：后者要伪造一个"比当前
// 二进制新"的数据库，只能靠手写 schema_versions 行，与本函数的判定逻辑完全重复，
// 测不出额外信息。
func TestGuardSchemaDowngrade(t *testing.T) {
	entries := dirEntriesOf(t, "001_a.sql", "002_b.sql", "003_c.sql")
	s := &SQLiteStore{}

	t.Run("库版本落后于二进制_放行", func(t *testing.T) {
		if err := s.guardSchemaDowngrade(entries, map[int]bool{1: true}); err != nil {
			t.Fatalf("库只应用到 v001、二进制有 v003，应放行，实际拒绝: %v", err)
		}
	})

	t.Run("库版本与二进制持平_放行", func(t *testing.T) {
		if err := s.guardSchemaDowngrade(entries, map[int]bool{1: true, 2: true, 3: true}); err != nil {
			t.Fatalf("版本持平应放行，实际拒绝: %v", err)
		}
	})

	t.Run("空库_放行", func(t *testing.T) {
		if err := s.guardSchemaDowngrade(entries, map[int]bool{}); err != nil {
			t.Fatalf("首次启动（无已应用版本）应放行，实际拒绝: %v", err)
		}
	})

	t.Run("库版本高于二进制_拒绝启动", func(t *testing.T) {
		err := s.guardSchemaDowngrade(entries, map[int]bool{1: true, 2: true, 3: true, 4: true})
		if err == nil {
			t.Fatal("库已应用到 v004 而二进制只到 v003（旧二进制 + 新库），必须拒绝启动")
		}
		// 错误信息要能让用户自己定位问题，而不是只说"启动失败"
		for _, want := range []string{"004", "003", "降级"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误信息缺少关键定位信息 %q: %v", want, err)
			}
		}
	})
}

func dirEntriesOf(t *testing.T, names ...string) []fs.DirEntry {
	t.Helper()
	m := fstest.MapFS{}
	for _, n := range names {
		m[n] = &fstest.MapFile{Data: []byte("-- test")}
	}
	entries, err := fs.ReadDir(m, ".")
	if err != nil {
		t.Fatalf("构造测试 DirEntry 失败: %v", err)
	}
	return entries
}
