package agent

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func newLedgerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"039_llm_calls.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func insertLedgerCall(t *testing.T, db *sql.DB, id, session string, atMs int64, in, out int, cost float64) {
	t.Helper()
	if err := repo.NewSQLiteLLMCallRepository(db).RecordLLMUsage(context.Background(), protocol.LLMUsageRecord{
		ID: id, CreatedAtMs: atMs, SessionID: session, Provider: "p", Status: protocol.LLMUsageStatusOK,
		InputTokens: in, OutputTokens: out, CostUSD: cost,
	}); err != nil {
		t.Fatal(err)
	}
}

type fixedLimitSource struct {
	v     float64
	err   error
	calls int
}

func (f *fixedLimitSource) GetBudget(context.Context) (float64, error) { f.calls++; return f.v, f.err }

func TestSpendLedger_SessionTokensIsolated(t *testing.T) {
	db := newLedgerTestDB(t)
	insertLedgerCall(t, db, "a", "s1", 1, 100, 50, 0)
	insertLedgerCall(t, db, "b", "s1", 2, 10, 5, 0)
	insertLedgerCall(t, db, "c", "s2", 3, 9999, 9999, 0)
	l := repo.NewSQLiteLLMSpendLedger(db)
	if n, err := l.SessionTokens(context.Background(), "s1"); err != nil || n != 165 {
		t.Fatalf("s1=%d err=%v", n, err)
	}
	if n, _ := l.SessionTokens(context.Background(), "none"); n != 0 {
		t.Fatalf("none=%d", n)
	}
}

func TestBudgetManager_SessionBudgetFromLedger(t *testing.T) {
	db := newLedgerTestDB(t)
	ctx := context.Background()
	ledger := repo.NewSQLiteLLMSpendLedger(db)
	// 默认会话预算 5M：s1 超限，s2 不受 s1 影响。
	insertLedgerCall(t, db, "a", "s1", 1, 4_000_000, 1_500_000, 0)
	insertLedgerCall(t, db, "b", "s2", 2, 100, 100, 0)
	over := NewBudgetManager().WithLedger("s1", ledger, nil)
	if err := over.ConsumeTokens(ctx, 10); err == nil {
		t.Fatal("s1 应超限")
	}
	ok := NewBudgetManager().WithLedger("s2", ledger, nil)
	if err := ok.ConsumeTokens(ctx, 10); err != nil {
		t.Fatalf("s2 不应受 s1 用量影响: %v", err)
	}
	// 重启即新建 BudgetManager，用量仍在账本里。
	again := NewBudgetManager().WithLedger("s1", ledger, nil)
	if err := again.ConsumeTokens(ctx, 0); err == nil {
		t.Fatal("重建后 s1 仍应超限")
	}
	// 空 sessionID 不做会话判定。
	if err := NewBudgetManager().WithLedger("", ledger, nil).ConsumeTokens(ctx, 1); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetManager_MonthlySpendBoundary(t *testing.T) {
	db := newLedgerTestDB(t)
	ctx := context.Background()
	jul31 := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC).UnixMilli()
	aug1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	insertLedgerCall(t, db, "a", "s1", jul31, 1, 1, 10.0)
	insertLedgerCall(t, db, "b", "s1", aug1, 1, 1, 2.0)
	insertLedgerCall(t, db, "c", "s2", aug1+1000, 1, 1, 0.5)

	bm := NewBudgetManager().WithLedger("s1", repo.NewSQLiteLLMSpendLedger(db), nil)
	bm.Now = func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }
	if got := bm.MonthlySpendUSD(ctx); got != 2.5 {
		t.Fatalf("8 月花费=%v want 2.5（含 8/1 0 点整、跨会话、不含 7 月）", got)
	}
	bm.Now = func() time.Time { return time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC) }
	if got := bm.MonthlySpendUSD(ctx); got != 12.5 {
		t.Fatalf("7 月边界=%v want 12.5", got)
	}
}

func TestBudgetManager_MonthlyLimitTTLAndZero(t *testing.T) {
	ctx := context.Background()
	src := &fixedLimitSource{v: 0}
	bm := NewBudgetManager().WithLedger("s", nil, src)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	bm.Now = func() time.Time { return now }

	if bm.MonthlyLimitUSD(ctx) != 0 {
		t.Fatal("0 = 不限额")
	}
	src.v = 50
	now = now.Add(10 * time.Second)
	if bm.MonthlyLimitUSD(ctx) != 0 || src.calls != 1 {
		t.Fatalf("TTL 内应命中缓存: calls=%d", src.calls)
	}
	now = now.Add(21 * time.Second)
	if got := bm.MonthlyLimitUSD(ctx); got != 50 || src.calls != 2 {
		t.Fatalf("TTL 过期应重读: got=%v calls=%d", got, src.calls)
	}
	// 读失败沿用旧值。
	src.err = errors.New("db down")
	now = now.Add(time.Minute)
	if got := bm.MonthlyLimitUSD(ctx); got != 50 {
		t.Fatalf("失败应沿用缓存: %v", got)
	}
}

func TestBudgetManager_LedgerFailureFailsOpen(t *testing.T) {
	db := newLedgerTestDB(t)
	db.Close() // 账本不可用
	bm := NewBudgetManager().WithLedger("s1", repo.NewSQLiteLLMSpendLedger(db), &fixedLimitSource{err: errors.New("x")})
	ctx := context.Background()
	if err := bm.ConsumeTokens(ctx, 1); err != nil {
		t.Fatalf("账本故障应放行: %v", err)
	}
	if bm.MonthlySpendUSD(ctx) != 0 || bm.MonthlyLimitUSD(ctx) != 0 {
		t.Fatal("故障时花费与上限均按 0")
	}
}
