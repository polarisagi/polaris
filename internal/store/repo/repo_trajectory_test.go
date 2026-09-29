package repo

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newTrajectoryTestRepo(t *testing.T) *SQLiteTrajectoryRepository {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("045_session_trajectory.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return NewSQLiteTrajectoryRepository(db)
}

func boolp(b bool) *bool { return &b }

func TestTrajectory_SeqPerSessionAndNullColumns(t *testing.T) {
	ctx := context.Background()
	r := newTrajectoryTestRepo(t)
	for i, sid := range []string{"a", "a", "b", "a"} {
		seq, err := r.Append(ctx, sid, "3", "", nil, -1, "{}")
		if err != nil {
			t.Fatal(err)
		}
		want := map[int]int64{0: 1, 1: 2, 2: 1, 3: 3}[i]
		if seq != want {
			t.Fatalf("append %d(%s): seq=%d want %d", i, sid, seq, want)
		}
	}
	rows, _ := r.ListBySession(ctx, "a")
	if len(rows) != 3 || rows[0].ToolName != "" || rows[0].ToolOK != nil || rows[0].LatencyMs != -1 {
		t.Fatalf("非工具行的工具列应为 NULL: %+v", rows)
	}
	if none, _ := r.ListBySession(ctx, "nope"); len(none) != 0 {
		t.Fatalf("unknown session: %+v", none)
	}
}

func TestTrajectory_ConcurrentAppendSeqUnique(t *testing.T) {
	ctx := context.Background()
	r := newTrajectoryTestRepo(t)
	const workers, per = 8, 25
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				if _, err := r.Append(ctx, "s", "tool_call", "t", boolp(true), 1, "{}"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	rows, err := r.ListBySession(ctx, "s")
	if err != nil || len(rows) != workers*per {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	for i, row := range rows {
		if row.Seq != int64(i+1) {
			t.Fatalf("seq 应从 1 连续单调无重复: idx=%d seq=%d", i, row.Seq)
		}
	}
}

func TestTrajectory_ToolQueries(t *testing.T) {
	ctx := context.Background()
	r := newTrajectoryTestRepo(t)
	app := func(sid, ev, tool string, ok bool) {
		var tp *bool
		lat := int64(-1)
		if tool != "" {
			tp, lat = boolp(ok), 5
		}
		if _, err := r.Append(ctx, sid, ev, tool, tp, lat, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	app("s1", "3", "", false)
	app("s1", "tool_call", "read", true)
	app("s1", "tool_call", "bash", false)
	app("s1", "tool_call", "read", true)
	app("s2", "tool_call", "read", false)
	app("s2", "tool_call", "edit", true)

	tools, _ := r.ListToolBySession(ctx, "s1")
	if len(tools) != 3 || tools[0].ToolName != "read" || tools[1].ToolOK == nil || *tools[1].ToolOK || tools[0].LatencyMs != 5 {
		t.Fatalf("ListToolBySession: %+v", tools)
	}

	// read 共 3 条，取每工具最近 2 条 → read 只剩 s1 第二条与 s2；结果按 id 升序。
	recent, _ := r.RecentToolRows(ctx, 2)
	count := map[string]int{}
	for i, row := range recent {
		count[row.ToolName]++
		if i > 0 && recent[i-1].ID >= row.ID {
			t.Fatalf("应按 id 升序: %+v", recent)
		}
	}
	if count["read"] != 2 || count["bash"] != 1 || count["edit"] != 1 || len(recent) != 4 {
		t.Fatalf("RecentToolRows: %v", count)
	}
	if none, _ := r.RecentToolRows(ctx, 0); none != nil {
		t.Fatalf("perTool<=0 应返回空: %+v", none)
	}

	seqs, err := r.RecentToolSequences(ctx, 10, 0)
	if err != nil || len(seqs) != 2 {
		t.Fatalf("RecentToolSequences: %v %v", seqs, err)
	}
	got := map[int][]string{len(seqs[0]): seqs[0], len(seqs[1]): seqs[1]}
	if g := got[3]; len(g) != 3 || g[0] != "read" || g[1] != "bash" || g[2] != "read" {
		t.Fatalf("s1 序列应按 seq 排序: %v", seqs)
	}
	if g := got[2]; len(g) != 2 || g[0] != "read" || g[1] != "edit" {
		t.Fatalf("s2 序列: %v", seqs)
	}
	// 只取最近 1 个会话（s2）。
	one, _ := r.RecentToolSequences(ctx, 1, 0)
	if len(one) != 1 || len(one[0]) != 2 {
		t.Fatalf("maxSessions=1 应只含最近会话: %v", one)
	}
	// sinceMs 晚于所有行 → 空。
	if far, _ := r.RecentToolSequences(ctx, 10, 1<<60); len(far) != 0 {
		t.Fatalf("sinceMs 过滤: %v", far)
	}
}
