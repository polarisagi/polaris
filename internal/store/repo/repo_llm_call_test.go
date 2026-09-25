package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

// 直接用 SSoT DDL 建表：列清单漂移时 INSERT 失败即暴露。
func TestLLMCallRepository_RecordRoundTrip(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("039_llm_calls.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}

	rec := protocol.LLMUsageRecord{
		ID: "c1", CreatedAtMs: 1, SessionID: "s1", Purpose: "plan", Provider: "deepseek-flash",
		ModelID: "deepseek-flash", ModelPool: "default", ThinkingMode: "high", Streaming: true,
		Status: protocol.LLMUsageStatusOK, InputTokens: 1000, CacheHitTokens: 800, OutputTokens: 300,
		ReasoningTokens: 200, LatencyMs: 1200, CostUSD: 0.0005,
	}
	if err := NewSQLiteLLMCallRepository(db).RecordLLMUsage(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	var model, purpose string
	var in, hit, out, reasoning, streaming int
	if err := db.QueryRow(`SELECT model_id, purpose, input_tokens, cache_hit_tokens, output_tokens, reasoning_tokens, streaming
		FROM llm_calls WHERE id='c1'`).Scan(&model, &purpose, &in, &hit, &out, &reasoning, &streaming); err != nil {
		t.Fatal(err)
	}
	if model != "deepseek-flash" || purpose != "plan" || in != 1000 || hit != 800 || out != 300 || reasoning != 200 || streaming != 1 {
		t.Fatalf("round trip mismatch: %s %s %d %d %d %d %d", model, purpose, in, hit, out, reasoning, streaming)
	}
}
