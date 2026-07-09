package consolidation

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const eventsSchemaDDL = `
CREATE TABLE IF NOT EXISTS events (
    offset            INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT NOT NULL UNIQUE,
    topic             TEXT NOT NULL,
    actor             TEXT NOT NULL,
    type              TEXT NOT NULL,
    payload           BLOB NOT NULL,
    idempotency_key   TEXT UNIQUE,
    embedding         BLOB,
    memory_layer      TEXT DEFAULT 'episodic',
    salience          REAL DEFAULT 0.5,
    occurred_at       INTEGER,
    durative_group_id TEXT,
    created_at        INTEGER NOT NULL,
    metadata          TEXT,
    prev_hash         TEXT,
    hash              TEXT
);`

// newTestEventsDB 创建一个带 events 表的临时 SQLite 库（文件形式，以便 ATTACH DATABASE
// 能正常工作 —— 部分 SQLite 驱动对 :memory: 主库 ATTACH 文件库有限制，直接用临时文件
// 与生产环境行为一致）。
func newTestEventsDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "events.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(eventsSchemaDDL); err != nil {
		t.Fatalf("create events table: %v", err)
	}
	return db
}

// insertTestEvent 插入一条 created_at 为 ageDays 天前的事件。
func insertTestEvent(t *testing.T, db *sql.DB, id string, ageDays int) {
	t.Helper()
	createdAt := time.Now().AddDate(0, 0, -ageDays).UnixMilli()
	_, err := db.Exec(
		`INSERT INTO events (id, topic, actor, type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, "test.topic", "system:test", "system", []byte("payload"), createdAt,
	)
	if err != nil {
		t.Fatalf("insert event %s: %v", id, err)
	}
}

func countEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestEventArchiver_SkipsWhenDiskHealthy 验证 Tier-0 磁盘水位门控（P1-3）核心行为：
// 空闲磁盘占比高于 watermark 时，Archive() 直接跳过，不做任何 ATTACH/INSERT/DELETE，
// 老事件原样保留在主库。
func TestEventArchiver_SkipsWhenDiskHealthy(t *testing.T) {
	db := newTestEventsDB(t)
	insertTestEvent(t, db, "old-1", 60) // 60 天前，超过默认 30 天 warmDays

	ea := NewEventArchiver(db, 30, t.TempDir(), 20) // watermark 20%
	ea.diskFreeRatioFn = func(string) (float64, bool) { return 0.50, true }

	if err := ea.Archive(context.Background()); err != nil {
		t.Fatalf("Archive failed: %v", err)
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("expected old event to remain untouched when disk healthy, got %d rows", n)
	}
}

// TestEventArchiver_ArchivesWhenDiskLow 验证磁盘紧张（空闲占比低于 watermark）时
// Archive() 正常执行归档+删除。
func TestEventArchiver_ArchivesWhenDiskLow(t *testing.T) {
	db := newTestEventsDB(t)
	insertTestEvent(t, db, "old-1", 60)
	insertTestEvent(t, db, "recent-1", 1) // 1 天前，未过 warmDays，不应被归档

	coldDir := t.TempDir()
	ea := NewEventArchiver(db, 30, coldDir, 20) // watermark 20%
	ea.diskFreeRatioFn = func(string) (float64, bool) { return 0.05, true }

	if err := ea.Archive(context.Background()); err != nil {
		t.Fatalf("Archive failed: %v", err)
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("expected only recent event to remain in main db, got %d rows", n)
	}

	coldDB, err := sql.Open("sqlite", filepath.Join(coldDir, "events_archive.db"))
	if err != nil {
		t.Fatalf("open cold db: %v", err)
	}
	defer coldDB.Close()
	var coldCount int
	if err := coldDB.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&coldCount); err != nil {
		t.Fatalf("count cold events: %v", err)
	}
	if coldCount != 1 {
		t.Errorf("expected 1 archived event in cold db, got %d", coldCount)
	}
}

// TestEventArchiver_ProbeFailure_FailsOpen 验证磁盘探针失败（ok=false）时
// fail-open：不因探针故障导致归档永久停摆，退回无条件执行。
func TestEventArchiver_ProbeFailure_FailsOpen(t *testing.T) {
	db := newTestEventsDB(t)
	insertTestEvent(t, db, "old-1", 60)

	ea := NewEventArchiver(db, 30, t.TempDir(), 20)
	ea.diskFreeRatioFn = func(string) (float64, bool) { return 0, false } // 探测失败

	if err := ea.Archive(context.Background()); err != nil {
		t.Fatalf("Archive failed: %v", err)
	}
	if n := countEvents(t, db); n != 0 {
		t.Errorf("expected archive to proceed (fail-open) when probe fails, got %d rows remaining", n)
	}
}

// TestEventArchiver_WatermarkDisabled 验证 diskWatermarkPct<=0 时门控被禁用，
// 等价于旧版无条件归档行为（向后兼容）。
func TestEventArchiver_WatermarkDisabled(t *testing.T) {
	db := newTestEventsDB(t)
	insertTestEvent(t, db, "old-1", 60)

	ea := NewEventArchiver(db, 30, t.TempDir(), 0) // 门控禁用
	ea.diskFreeRatioFn = func(string) (float64, bool) { return 0.99, true }

	if err := ea.Archive(context.Background()); err != nil {
		t.Fatalf("Archive failed: %v", err)
	}
	if n := countEvents(t, db); n != 0 {
		t.Errorf("expected unconditional archive when watermark disabled, got %d rows remaining", n)
	}
}
