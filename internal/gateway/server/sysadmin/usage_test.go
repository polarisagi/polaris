package sysadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
)

var usageNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func newUsageHandler(t *testing.T) *SysAdminHandler {
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
	w := repo.NewSQLiteLLMCallRepository(db)
	rows := []protocol.LLMUsageRecord{
		{ID: "1", CreatedAtMs: usageNow.Add(-2 * time.Hour).UnixMilli(), Purpose: "plan", Provider: "ds", ModelID: "flash", Status: "ok", InputTokens: 1000, CacheHitTokens: 900, OutputTokens: 200, ReasoningTokens: 100, CostUSD: 0.4},
		{ID: "2", CreatedAtMs: usageNow.Add(-3 * time.Hour).UnixMilli(), Purpose: "plan", Provider: "ds", ModelID: "flash", Status: "ok", InputTokens: 1000, CacheHitTokens: 100, OutputTokens: 100, CostUSD: 0.2},
		{ID: "3", CreatedAtMs: usageNow.Add(-1 * time.Hour).UnixMilli(), Purpose: "graphrag_extract", Provider: "ds", ModelID: "flash", Status: protocol.LLMUsageStatusCacheHit},
		{ID: "4", CreatedAtMs: usageNow.Add(-72 * time.Hour).UnixMilli(), Purpose: "respond", Provider: "oa", ModelID: "mini", Status: "ok", InputTokens: 50, OutputTokens: 5, CostUSD: 0.01},
	}
	for _, r := range rows {
		if err := w.RecordLLMUsage(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	return &SysAdminHandler{UsageRepo: repo.NewSQLiteLLMUsageQuery(db), nowFn: func() time.Time { return usageNow }}
}

func getUsage(t *testing.T, h *SysAdminHandler, query string) (*httptest.ResponseRecorder, UsageReport) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/usage"+query, nil)
	rec := httptest.NewRecorder()
	h.HandleGetUsage(rec, req)
	var rep UsageReport
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, rep
}

// 默认：最近 24h、按 purpose 分组；72h 前的行不计入；缓存命中率 = cache_hit_tokens/input_tokens；
// cache_hit 状态行计入请求数与 response_cache_hits，但不进 token 分母。
func TestHandleGetUsage_DefaultsLast24hByPurpose(t *testing.T) {
	rec, rep := getUsage(t, newUsageHandler(t), "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rep.GroupBy != "purpose" || len(rep.Groups) != 2 {
		t.Fatalf("默认按 purpose 且排除 72h 前的行: %+v", rep)
	}
	plan := rep.Groups[0]
	if plan.Key != "plan" || plan.Requests != 2 || plan.InputTokens != 2000 || plan.CacheHitTokens != 1000 ||
		plan.OutputTokens != 300 || plan.ReasoningTokens != 100 || plan.CacheHitRatio != 0.5 || plan.CostUSD < 0.599 || plan.CostUSD > 0.601 {
		t.Fatalf("plan: %+v", plan)
	}
	ex := rep.Groups[1]
	if ex.Key != "graphrag_extract" || ex.Requests != 1 || ex.ResponseCacheHits != 1 || ex.CacheHitRatio != 0 || ex.InputTokens != 0 {
		t.Fatalf("cache_hit 行: %+v", ex)
	}
	if rep.Total.Key != "total" || rep.Total.Requests != 3 || rep.Total.ResponseCacheHits != 1 || rep.Total.CacheHitRatio != 0.5 {
		t.Fatalf("total: %+v", rep.Total)
	}
	if rep.Since != usageNow.Add(-24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("since=%s", rep.Since)
	}
}

func TestHandleGetUsage_GroupByAndRelativeSince(t *testing.T) {
	h := newUsageHandler(t)
	_, rep := getUsage(t, h, "?since=7d&group_by=day")
	if rep.GroupBy != "day" || len(rep.Groups) != 2 || rep.Groups[0].Key != "2026-09-27" || rep.Groups[1].Key != "2026-09-30" {
		t.Fatalf("7d 应含 72h 前那天且按日升序: %+v", rep.Groups)
	}
	_, rep = getUsage(t, h, "?since=7d&group_by=provider")
	if len(rep.Groups) != 2 || rep.Total.Requests != 4 {
		t.Fatalf("provider: %+v", rep)
	}
	_, rep = getUsage(t, h, "?since=7d&group_by=model")
	if len(rep.Groups) != 2 {
		t.Fatalf("model: %+v", rep)
	}
	// RFC3339 绝对时间 + until 相对时间："since=绝对, until=2h" => [since, now-2h)。
	since := usageNow.Add(-5 * time.Hour).Format(time.RFC3339)
	_, rep = getUsage(t, h, "?since="+since+"&until=150m")
	if rep.Total.Requests != 1 || rep.Groups[0].Key != "plan" { // 只有 3h 前那条 plan 落在 [-5h, -2.5h)
		t.Fatalf("绝对 since + 相对 until: %+v", rep)
	}
}

func TestHandleGetUsage_BadParams(t *testing.T) {
	h := newUsageHandler(t)
	for _, q := range []string{
		"?group_by=session", "?group_by=purpose%3BDROP", "?since=yesterday", "?since=-5h", "?until=abc",
		"?since=1h&until=2h", // since 晚于 until
	} {
		if rec, _ := getUsage(t, h, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s => %d, want 400", q, rec.Code)
		}
	}
}

func TestHandleGetUsage_NoRepo503(t *testing.T) {
	if rec, _ := getUsage(t, &SysAdminHandler{}, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配账本应 503，got %d", rec.Code)
	}
}

// 旧口径历史行可能使 cache_hit > input：比率夹到 1，不得出现 >100%。
func TestToUsageView_RatioClamped(t *testing.T) {
	v := toUsageView(repoGroup(100, 900))
	if v.CacheHitRatio != 1 {
		t.Fatalf("ratio=%v", v.CacheHitRatio)
	}
	if toUsageView(repoGroup(0, 0)).CacheHitRatio != 0 {
		t.Fatal("无输入时比率应为 0")
	}
}

func repoGroup(input, hit int64) protorepo.LLMUsageGroup {
	return protorepo.LLMUsageGroup{InputTokens: input, CacheHitTokens: hit}
}

func newSessionUsageHandler(t *testing.T) *SysAdminHandler {
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
	w := repo.NewSQLiteLLMCallRepository(db)
	for i, r := range []protocol.LLMUsageRecord{
		{ID: "a", CreatedAtMs: 1000, SessionID: "sess_abc", TurnID: "turn_1", Purpose: "kernel", ModelID: "deepseek-flash", Status: "ok", InputTokens: 20000, CacheHitTokens: 15000, OutputTokens: 500, CostUSD: 0.005},
		{ID: "b", CreatedAtMs: 2000, SessionID: "sess_abc", TurnID: "turn_1", Purpose: "kernel", ModelID: "deepseek-flash", Status: "ok", InputTokens: 100000, CacheHitTokens: 20000, OutputTokens: 100, CostUSD: 0.03},
		{ID: "c", CreatedAtMs: 3000, SessionID: "sess_other", Purpose: "kernel", ModelID: "deepseek-flash", Status: "ok", InputTokens: 1, CostUSD: 7},
	} {
		_ = i
		if err := w.RecordLLMUsage(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	return &SysAdminHandler{UsageRepo: repo.NewSQLiteLLMUsageQuery(db)}
}

func getSessionUsage(h *SysAdminHandler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x/usage", nil)
	req.SetPathValue("sessionID", id)
	rec := httptest.NewRecorder()
	h.HandleGetSessionUsage(rec, req)
	return rec
}

func TestHandleGetSessionUsage(t *testing.T) {
	h := newSessionUsageHandler(t)
	rec := getSessionUsage(h, "sess_abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var v SessionUsageView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Calls != 2 || v.Input != 120000 || v.CacheHit != 35000 || v.Output != 600 || v.CostUSD < 0.0349 || v.CostUSD > 0.0351 {
		t.Fatalf("累计不对（不得混入其他会话）: %+v", v)
	}
	// 最近一次内核请求 10 万 token，deepseek-flash 窗口 100 万（目录表缺失时回退价目目录）。
	if v.LastInput != 100000 || v.ContextWindow != 1_000_000 || v.ContextPct < 0.0999 || v.ContextPct > 0.1001 {
		t.Fatalf("上下文占用: %+v", v)
	}
	if len(v.ByTurn) != 1 || v.ByTurn[0].TurnID != "turn_1" || v.ByTurn[0].Calls != 2 {
		t.Fatalf("by_turn: %+v", v.ByTurn)
	}
	if rec := getSessionUsage(h, "bad id!"); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 session_id 应 400，得到 %d", rec.Code)
	}
	if rec := getSessionUsage(&SysAdminHandler{}, "sess_abc"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无账本应 503，得到 %d", rec.Code)
	}
	// 无记录的会话返回零值而非 404。
	var z SessionUsageView
	rec = getSessionUsage(h, "sess_none")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &z) != nil || z.Calls != 0 || z.ByTurn == nil {
		t.Fatalf("空会话: %d %s", rec.Code, rec.Body.String())
	}
}
