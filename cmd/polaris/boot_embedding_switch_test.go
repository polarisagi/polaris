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
	if _, err := db.Exec(`CREATE TABLE preferences (key TEXT PRIMARY KEY, value TEXT NOT NULL, expired_at INTEGER)`); err != nil {
		t.Fatal(err)
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
