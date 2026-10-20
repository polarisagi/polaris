package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, n)
	}
	sort.Strings(cols)
	return cols
}

func objectExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = ?", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// 存量库（没有新增列/表）经 upgradeLegacySchema 补齐后，结构必须与全新建库完全一致。
func TestUpgradeLegacySchema_MatchesFreshSchema(t *testing.T) {
	fresh, err := OpenSQLite(filepath.Join(t.TempDir(), "fresh.db"), schema.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	// 构造"旧库"：全新建库后去掉本次新增的列与表，再回放补齐。
	old, err := OpenSQLite(filepath.Join(t.TempDir(), "old.db"), schema.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	for _, stmt := range []string{
		"DROP INDEX IF EXISTS idx_llm_calls_turn",
		"ALTER TABLE llm_calls DROP COLUMN turn_id",
		"ALTER TABLE chat_messages DROP COLUMN turn_id",
		"ALTER TABLE chat_messages DROP COLUMN usage",
		"DROP TABLE chat_turn_steps",
	} {
		if _, err := old.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.upgradeLegacySchema(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	// 幂等：再跑一次不报错。
	if err := old.upgradeLegacySchema(); err != nil {
		t.Fatalf("upgrade 第二次: %v", err)
	}
	for _, tb := range []string{"llm_calls", "chat_messages", "chat_turn_steps"} {
		a, b := strings.Join(tableColumns(t, fresh.db, tb), ","), strings.Join(tableColumns(t, old.db, tb), ",")
		if a != b {
			t.Errorf("%s 列不一致\n fresh: %s\n  old : %s", tb, a, b)
		}
	}
	for _, idx := range []string{"idx_chat_turn_steps_session", "idx_llm_calls_turn"} {
		if !objectExists(t, old.db, idx) {
			t.Errorf("存量库缺少索引 %s", idx)
		}
	}
}

func seedCalls(t *testing.T, db *sql.DB) {
	t.Helper()
	// 两条真实事故记录 + 一条目录外模型。
	for _, r := range []struct {
		id, model    string
		in, hit, out int
		cost         float64
	}{
		{"a", "deepseek-flash", 11762, 0, 322, 1.9575},
		{"b", "deepseek-flash", 24612, 14976, 2444, 2.91},
		{"c", "some-unknown-model", 1000, 0, 100, 0.5},
	} {
		if _, err := db.Exec(`INSERT INTO llm_calls(id, created_at, provider, model_id, status, input_tokens, cache_hit_tokens, output_tokens, cost_usd)
			VALUES(?, 1, 'p', ?, 'ok', ?, ?, ?, ?)`, r.id, r.model, r.in, r.hit, r.out, r.cost); err != nil {
			t.Fatal(err)
		}
	}
}

func costOf(t *testing.T, db *sql.DB, id string) float64 {
	t.Helper()
	var c float64
	if err := db.QueryRow("SELECT cost_usd FROM llm_calls WHERE id = ?", id).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRepriceLLMCalls_RepricesIdempotentlyAndReverts(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "p.db"), schema.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedCalls(t, s.db)

	// OpenSQLite 时库还是空的，标记已写；清掉标记模拟"升级前就有历史行的库"。
	if _, err := s.db.Exec("DELETE FROM sys_config WHERE key = ?", repriceMarkerKey); err != nil {
		t.Fatal(err)
	}
	n, err := RepriceLLMCalls(s.db, 123)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v 期望重算 2 行（目录外模型不动）", n, err)
	}
	if a := costOf(t, s.db, "a"); a < 0.0035 || a > 0.0040 {
		t.Errorf("a 重算后 %v", a)
	}
	if b := costOf(t, s.db, "b"); b < 0.0058 || b > 0.0066 {
		t.Errorf("b 重算后 %v", b)
	}
	if c := costOf(t, s.db, "c"); c != 0.5 {
		t.Errorf("目录外模型被改动: %v", c)
	}
	// 幂等：第二次不再处理。
	if n, err := RepriceLLMCalls(s.db, 456); err != nil || n != 0 {
		t.Fatalf("第二次应跳过: n=%d err=%v", n, err)
	}
	// 可逆。
	r, err := RevertLLMCostReprice(s.db)
	if err != nil || r != 2 {
		t.Fatalf("revert r=%d err=%v", r, err)
	}
	if a := costOf(t, s.db, "a"); a != 1.9575 {
		t.Errorf("还原后 a=%v", a)
	}
	if b := costOf(t, s.db, "b"); b != 2.91 {
		t.Errorf("还原后 b=%v", b)
	}
	// 还原后可再次重算（标记已清）。
	if n, _ := RepriceLLMCalls(s.db, 789); n != 2 {
		t.Fatalf("还原后再重算 n=%d", n)
	}
}
