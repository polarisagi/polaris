package learning

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestLoadCursors_ScanFailure_AbortsAndReturnsError_S02 验证阶段02修复
// （GR-7-001）：任一游标行 Scan 失败时，loadCursors 必须中止并返回错误，
// 不得像修复前那样忽略该行继续遍历——那会导致该 stream 的游标缺失，
// Start() 据此从 seq=0 重放整条流，产生与其他已加载流不一致的重放窗口。
func TestLoadCursors_ScanFailure_AbortsAndReturnsError_S02(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE learning_cursors (
		stream_name TEXT PRIMARY KEY,
		last_seq    TEXT NOT NULL,
		updated_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// last_seq 列在真实 schema 中是 INTEGER，这里故意放入一条非数字文本，
	// 使 Scan 到 int64 目标时失败，模拟游标行损坏场景。
	if _, err := db.Exec(`INSERT INTO learning_cursors (stream_name, last_seq, updated_at) VALUES
		('task', '5', 0),
		('version', 'not_a_number', 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	taskEvents := make(chan TaskCompleteEvent, 1)
	versionEvents := make(chan VersionChangeEvent, 1)
	e := NewEngine(DefaultEngineConfig(), nil, nil, nil, taskEvents, versionEvents)
	e.SetDB(db)

	cursors, err := e.loadCursors(context.Background())
	if err == nil {
		t.Fatal("expected error when a cursor row fails to scan, got nil")
	}
	if cursors != nil {
		t.Errorf("expected nil cursors map on scan failure, got %v", cursors)
	}
}

// TestLoadCursors_AllValid_ReturnsAllRows_S02 正常路径回归锚点：确保修复后
// 全部合法行仍能被正确加载（防止中止逻辑误伤正常场景）。
func TestLoadCursors_AllValid_ReturnsAllRows_S02(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE learning_cursors (
		stream_name TEXT PRIMARY KEY,
		last_seq    INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO learning_cursors (stream_name, last_seq, updated_at) VALUES
		('task', 5, 0), ('version', 3, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	taskEvents := make(chan TaskCompleteEvent, 1)
	versionEvents := make(chan VersionChangeEvent, 1)
	e := NewEngine(DefaultEngineConfig(), nil, nil, nil, taskEvents, versionEvents)
	e.SetDB(db)

	cursors, err := e.loadCursors(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cursors["task"] != 5 || cursors["version"] != 3 {
		t.Errorf("expected task=5,version=3, got %v", cursors)
	}
}
