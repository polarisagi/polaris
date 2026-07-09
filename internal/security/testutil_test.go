package security

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

// openTestDB 创建内存 SQLite 并运行全部 migration
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 运行 schema migration
	files, err := schema.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("failed to read schema dir: %v", err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".sql") {
			continue
		}
		data, err := schema.FS.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("failed to read %s: %v", f.Name(), err)
		}
		_, err = db.Exec(string(data))
		if err != nil {
			t.Fatalf("failed to execute %s: %v", f.Name(), err)
		}
	}
	return db
}
