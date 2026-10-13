package main

import (
	"database/sql"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/llm"
)

func newPrefDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, ddl := range []string{
		`CREATE TABLE preferences (key TEXT PRIMARY KEY, value TEXT NOT NULL, expired_at INTEGER)`,
		// 最小表结构（照 schema 009 / 003 的列名），供 ADR-0113 决策六的版本戳置空断言。
		`CREATE TABLE rag_chunks (id TEXT PRIMARY KEY, embed_model_version TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE episodic_events (id TEXT PRIMARY KEY, embed_model_version TEXT NOT NULL DEFAULT '')`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func activeModel(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT value FROM preferences WHERE key = ?`, prefKeyActiveEmbedModel).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return v
}

// ADR-0109 P4：同维换模型（Gemma@512 ↔ bge@512）必须清空向量库；首次设置与同模型重设不清空；
// 清空失败时不得记录新版本（下次重试，绝不在旧向量仍在时宣称已切换）。
func TestEmbedModelSwitchHandler(t *testing.T) {
	db := newPrefDB(t)
	clears := 0
	var clearErr error
	h := newEmbedModelSwitchHandler(db, func() error { clears++; return clearErr })

	h("onnx:embeddinggemma-300m-q8@512") // 首次：只记录
	if clears != 0 || activeModel(t, db) != "onnx:embeddinggemma-300m-q8@512" {
		t.Fatalf("first set: clears=%d active=%q", clears, activeModel(t, db))
	}
	h("onnx:embeddinggemma-300m-q8@512") // 同模型：不清空
	if clears != 0 {
		t.Fatalf("same model must not clear, clears=%d", clears)
	}

	clearErr = errors.New("boom")
	h("onnx:bge-small-zh-v1.5-int8@512") // 切换但清空失败：保留旧记录
	if clears != 1 || activeModel(t, db) != "onnx:embeddinggemma-300m-q8@512" {
		t.Fatalf("failed clear must keep old version: clears=%d active=%q", clears, activeModel(t, db))
	}

	clearErr = nil
	h("onnx:bge-small-zh-v1.5-int8@512") // 重试成功
	if clears != 2 || activeModel(t, db) != "onnx:bge-small-zh-v1.5-int8@512" {
		t.Fatalf("switch: clears=%d active=%q", clears, activeModel(t, db))
	}

	h("") // 未就绪版本：忽略
	if clears != 2 || activeModel(t, db) != "onnx:bge-small-zh-v1.5-int8@512" {
		t.Fatal("empty version must be ignored")
	}
}

func versionCounts(t *testing.T, db *sql.DB, table string) (stamped int) {
	t.Helper()
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE embed_model_version != ''`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	return stamped
}

func seedStamps(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO rag_chunks (id, embed_model_version) VALUES ('r1','A'),('r2','')`,
		`INSERT INTO episodic_events (id, embed_model_version) VALUES ('e1','A'),('e2','B')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// ADR-0113 决策六：向量清空成功 ⇒ 两表版本戳置空，且记录新版本。
func TestEmbedModelSwitchHandler_ResetsVersionStampsAfterClear(t *testing.T) {
	db := newPrefDB(t)
	seedStamps(t, db)
	if _, err := db.Exec(`INSERT INTO preferences (key, value) VALUES (?, 'A')`, prefKeyActiveEmbedModel); err != nil {
		t.Fatal(err)
	}
	h := newEmbedModelSwitchHandler(db, func() error { return nil })
	h("B")
	if n := versionCounts(t, db, "rag_chunks") + versionCounts(t, db, "episodic_events"); n != 0 {
		t.Fatalf("stamps left after clear: %d", n)
	}
	if activeModel(t, db) != "B" {
		t.Fatalf("active = %q, want B", activeModel(t, db))
	}
}

// 无 SurrealDB（vecClear==nil）：SQLite 内向量仍有效，不得置空版本戳。
func TestEmbedModelSwitchHandler_NilClearKeepsStamps(t *testing.T) {
	db := newPrefDB(t)
	seedStamps(t, db)
	if _, err := db.Exec(`INSERT INTO preferences (key, value) VALUES (?, 'A')`, prefKeyActiveEmbedModel); err != nil {
		t.Fatal(err)
	}
	newEmbedModelSwitchHandler(db, nil)("B")
	if versionCounts(t, db, "rag_chunks") != 1 || versionCounts(t, db, "episodic_events") != 2 {
		t.Fatal("stamps must be kept when vecClear is nil")
	}
	if activeModel(t, db) != "B" {
		t.Fatalf("active = %q, want B", activeModel(t, db))
	}
}

// 置空失败按清空失败处理：不记录新版本。
func TestEmbedModelSwitchHandler_ResetFailureKeepsOldVersion(t *testing.T) {
	db := newPrefDB(t)
	seedStamps(t, db)
	if _, err := db.Exec(`INSERT INTO preferences (key, value) VALUES (?, 'A')`, prefKeyActiveEmbedModel); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE episodic_events`); err != nil {
		t.Fatal(err)
	}
	newEmbedModelSwitchHandler(db, func() error { return nil })("B")
	if activeModel(t, db) != "A" {
		t.Fatalf("active = %q, want A (new version must not be recorded)", activeModel(t, db))
	}
}

// 远程 / Ollama 后端不得被"重新评估"替换成 ONNX（用户配置优先）。
func TestTriggerRebench_OnlyForONNX(t *testing.T) {
	for _, kind := range []string{"remote", "ollama", "none"} {
		sb := &SubstrateBundle{EmbedChoice: embedChoice{Kind: kind}}
		sb.DynEmbedder = llm.NewDynamicEmbedder() // 引擎已初始化，确保拒绝来自后端类型判定
		if err := triggerRebench(t.Context(), sb); err == nil {
			t.Fatalf("kind=%s: rebench must be rejected", kind)
		}
	}
}
