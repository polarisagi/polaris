package learning

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestGetCursors_ScanFailure_AbortsAndReturnsError_S02 验证阶段02修复
// （GR-7-001）：任一游标行 Scan 失败时，loadCursors 必须中止并返回错误，
// 不得像修复前那样忽略该行继续遍历——那会导致该 stream 的游标缺失，
// Start() 据此从 seq=0 重放整条流，产生与其他已加载流不一致的重放窗口。
func TestGetCursors_ScanFailure_AbortsAndReturnsError_S02(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE consumer_cursors (
		consumer_id TEXT PRIMARY KEY,
		last_seq    TEXT NOT NULL,
		updated_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// last_seq 列在真实 schema 中是 INTEGER，这里故意放入一条非数字文本，
	// 使 Scan 到 int64 目标时失败，模拟游标行损坏场景。
	if _, err := db.Exec(`INSERT INTO consumer_cursors (consumer_id, last_seq, updated_at) VALUES
		('learning.task', '5', 0),
		('learning.version', 'not_a_number', 0)`); err != nil {
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

// TestGetCursors_AllValid_ReturnsAllRows_S02 正常路径回归锚点：确保修复后
// 全部合法行仍能被正确加载（防止中止逻辑误伤正常场景）。
func TestGetCursors_AllValid_ReturnsAllRows_S02(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE consumer_cursors (
		consumer_id TEXT PRIMARY KEY,
		last_seq    INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO consumer_cursors (consumer_id, last_seq, updated_at) VALUES
		('learning.task', 5, 0), ('learning.version', 3, 0)`); err != nil {
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

// TestLearningCursorID_Whitelist_And_LoadIgnoresUnknown 并表后原 CHECK 约束改在 Go 侧：
// 非法流被拒绝；加载时白名单外的 learning.* 行与其他消费者行不污染结果。
func TestLearningCursorID_Whitelist_And_LoadIgnoresUnknown(t *testing.T) {
	for _, ok := range []string{"task", "version", "heuristic", "eval"} {
		id, err := learningCursorID(ok)
		if err != nil || id != "learning."+ok {
			t.Errorf("stream %q: id=%q err=%v", ok, id, err)
		}
	}
	if _, err := learningCursorID("bogus"); err == nil {
		t.Error("非法 stream 应返回错误")
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE consumer_cursors (consumer_id TEXT PRIMARY KEY, last_seq INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO consumer_cursors VALUES ('learning.eval', 9, 0), ('learning.bogus', 1, 0), ('memory_agent.whisper', 77, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	e := NewEngine(DefaultEngineConfig(), nil, nil, nil, make(chan TaskCompleteEvent, 1), make(chan VersionChangeEvent, 1))
	e.SetDB(db)
	cursors, err := e.loadCursors(context.Background())
	if err != nil {
		t.Fatalf("loadCursors: %v", err)
	}
	if len(cursors) != 1 || cursors["eval"] != 9 {
		t.Errorf("want only eval=9, got %v", cursors)
	}
}
