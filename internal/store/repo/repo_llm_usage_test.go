package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newLLMCallsDB(t *testing.T) *sql.DB {
	t.Helper()
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
	return db
}

func ms(day, hour int) int64 { // 2026-09-<day> <hour>:00 UTC
	return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC).UnixMilli()
}

func seedUsage(t *testing.T, w *SQLiteLLMCallRepository, rows ...protocol.LLMUsageRecord) {
	t.Helper()
	for i := range rows {
		if rows[i].ID == "" {
			rows[i].ID = fmt.Sprintf("r%d", i)
		}
		if rows[i].Status == "" {
			rows[i].Status = protocol.LLMUsageStatusOK
		}
		if err := w.RecordLLMUsage(context.Background(), rows[i]); err != nil {
			t.Fatal(err)
		}
	}
}

func byKey(gs []protorepo.LLMUsageGroup) map[string]protorepo.LLMUsageGroup {
	m := map[string]protorepo.LLMUsageGroup{}
	for _, g := range gs {
		m[g.Key] = g
	}
	return m
}

// 聚合须区分：全部行数、cache_hit 状态行数、各类 token 与费用；cache_hit 行 token=0 但计入请求数。
func TestAggregateLLMUsage_GroupsAndCacheHitRows(t *testing.T) {
	db := newLLMCallsDB(t)
	w := NewSQLiteLLMCallRepository(db)
	seedUsage(t, w,
		protocol.LLMUsageRecord{CreatedAtMs: ms(29, 10), Purpose: "plan", Provider: "ds", ModelID: "flash", InputTokens: 1000, CacheHitTokens: 900, OutputTokens: 200, ReasoningTokens: 150, CostUSD: 0.5},
		protocol.LLMUsageRecord{CreatedAtMs: ms(29, 11), Purpose: "plan", Provider: "ds", ModelID: "flash", InputTokens: 500, CacheHitTokens: 0, OutputTokens: 50, CostUSD: 0.25},
		protocol.LLMUsageRecord{CreatedAtMs: ms(29, 12), Purpose: "graphrag_extract", Provider: "ds", ModelID: "flash", Status: protocol.LLMUsageStatusCacheHit},
		protocol.LLMUsageRecord{CreatedAtMs: ms(30, 1), Purpose: "graphrag_extract", Provider: "oa", ModelID: "mini", InputTokens: 300, CacheHitTokens: 100, OutputTokens: 30, CostUSD: 0.1},
		protocol.LLMUsageRecord{CreatedAtMs: ms(30, 2), Purpose: "respond", Provider: "oa", ModelID: "mini", Status: protocol.LLMUsageStatusError},
	)
	q := NewSQLiteLLMUsageQuery(db)
	ctx := context.Background()
	span := protorepo.LLMUsageQuery{SinceMs: ms(29, 0), UntilMs: ms(31, 0)}

	span.GroupBy = protorepo.LLMUsageByPurpose
	gs, err := q.AggregateLLMUsage(ctx, span)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 3 || gs[0].Key != "plan" { // 费用降序：plan 0.75 最大
		t.Fatalf("purpose 分组/排序: %+v", gs)
	}
	m := byKey(gs)
	plan := m["plan"]
	if plan.Requests != 2 || plan.InputTokens != 1500 || plan.CacheHitTokens != 900 || plan.OutputTokens != 250 ||
		plan.ReasoningTokens != 150 || plan.CostUSD != 0.75 || plan.ResponseCacheHits != 0 {
		t.Fatalf("plan: %+v", plan)
	}
	ex := m["graphrag_extract"]
	if ex.Requests != 2 || ex.ResponseCacheHits != 1 || ex.InputTokens != 300 || ex.CacheHitTokens != 100 {
		t.Fatalf("graphrag_extract 须计入 cache_hit 状态行（请求数 2，其中 1 行精确缓存命中）: %+v", ex)
	}
	if m["respond"].Requests != 1 || m["respond"].InputTokens != 0 {
		t.Fatalf("error 行计入请求数、token 为 0: %+v", m["respond"])
	}

	span.GroupBy = protorepo.LLMUsageByProvider
	if gs, _ = q.AggregateLLMUsage(ctx, span); byKey(gs)["ds"].Requests != 3 || byKey(gs)["oa"].Requests != 2 {
		t.Fatalf("provider 分组: %+v", gs)
	}
	span.GroupBy = protorepo.LLMUsageByModel
	if gs, _ = q.AggregateLLMUsage(ctx, span); byKey(gs)["flash"].InputTokens != 1500 || byKey(gs)["mini"].InputTokens != 300 {
		t.Fatalf("model 分组: %+v", gs)
	}
	span.GroupBy = protorepo.LLMUsageByDay // UTC 自然日、升序
	gs, _ = q.AggregateLLMUsage(ctx, span)
	if len(gs) != 2 || gs[0].Key != "2026-09-29" || gs[1].Key != "2026-09-30" || gs[0].Requests != 3 || gs[1].Requests != 2 {
		t.Fatalf("day 分组: %+v", gs)
	}
}

// 时间范围为 [since, until)：边界行的归属必须确定，且范围外与空范围不产生分组。
func TestAggregateLLMUsage_TimeRangeHalfOpen(t *testing.T) {
	db := newLLMCallsDB(t)
	seedUsage(t, NewSQLiteLLMCallRepository(db),
		protocol.LLMUsageRecord{CreatedAtMs: ms(29, 10), Purpose: "a", Provider: "p", InputTokens: 1},
		protocol.LLMUsageRecord{CreatedAtMs: ms(29, 11), Purpose: "b", Provider: "p", InputTokens: 2},
	)
	q := NewSQLiteLLMUsageQuery(db)
	gs, err := q.AggregateLLMUsage(context.Background(), protorepo.LLMUsageQuery{SinceMs: ms(29, 10), UntilMs: ms(29, 11), GroupBy: protorepo.LLMUsageByPurpose})
	if err != nil || len(gs) != 1 || gs[0].Key != "a" {
		t.Fatalf("since 含、until 不含: %+v %v", gs, err)
	}
	gs, err = q.AggregateLLMUsage(context.Background(), protorepo.LLMUsageQuery{SinceMs: ms(1, 0), UntilMs: ms(2, 0), GroupBy: protorepo.LLMUsageByPurpose})
	if err != nil || len(gs) != 0 {
		t.Fatalf("空范围应无分组: %+v %v", gs, err)
	}
}

func TestAggregateLLMUsage_RejectsUnknownGroupBy(t *testing.T) {
	q := NewSQLiteLLMUsageQuery(newLLMCallsDB(t))
	// 含注入形态的值必须在白名单处被拒绝，而不是进入 SQL。
	for _, g := range []protorepo.LLMUsageGroupBy{"", "session", "purpose; DROP TABLE llm_calls"} {
		if _, err := q.AggregateLLMUsage(context.Background(), protorepo.LLMUsageQuery{GroupBy: g}); err == nil {
			t.Fatalf("group_by %q 应被拒绝", g)
		}
	}
}

// 时间过滤必须走 created_at 索引，而不是全表扫描（llm_calls 是高增长表）。
func TestAggregateLLMUsage_UsesCreatedAtIndex(t *testing.T) {
	db := newLLMCallsDB(t)
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT purpose, COUNT(*) FROM llm_calls WHERE created_at >= ? AND created_at < ? GROUP BY purpose`, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "|"), "idx_llm_calls_created_at") {
		t.Fatalf("聚合应使用 created_at 索引: %v", plan)
	}
}

// 保留期清理：只删 created_at 早于阈值的行，跨批（>5000 行）也能删净，阈值边界行保留。
func TestPruneLLMCalls_DeletesOnlyOlderAcrossBatches(t *testing.T) {
	db := newLLMCallsDB(t)
	w := NewSQLiteLLMCallRepository(db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < llmCallsPruneBatch+123; i++ {
		if _, err := tx.Exec(`INSERT INTO llm_calls (id, created_at, provider, status) VALUES (?, ?, 'p', 'ok')`, fmt.Sprintf("old%d", i), int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO llm_calls (id, created_at, provider, status) VALUES ('edge', 10000, 'p', 'ok'), ('new', 20000, 'p', 'ok')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	n, err := w.PruneLLMCalls(context.Background(), 10000)
	if err != nil || n != int64(llmCallsPruneBatch+123) {
		t.Fatalf("应删 %d 行，got %d err=%v", llmCallsPruneBatch+123, n, err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM llm_calls`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("边界行(created_at==阈值)与新行应保留: left=%d err=%v", left, err)
	}
	// 幂等：再跑一次不删任何行。
	if n, err := w.PruneLLMCalls(context.Background(), 10000); err != nil || n != 0 {
		t.Fatalf("重复清理应为 0: %d %v", n, err)
	}
}

func TestPruneLLMCalls_CancelledContext(t *testing.T) {
	db := newLLMCallsDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSQLiteLLMCallRepository(db).PruneLLMCalls(ctx, 1); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}
}
