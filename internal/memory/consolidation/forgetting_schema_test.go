package consolidation

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

type recordingEdgeDeleter struct{ from []string }

func (r *recordingEdgeDeleter) GraphDeleteEdges(fromID, _ string) error {
	r.from = append(r.from, fromID)
	return nil
}

// GR-5.1-001 / GR-1.1-007：事务路径必须按 SSoT DDL 写 change_log（否则整条回滚、遗忘停摆），
// 归档后清理 episodic 节点图边，节点 ID 与 EpisodicGraphIndexer 写入侧一致。
func TestForgetting_TxPathMatchesSchemaAndCleansGraph(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ddl, err := schema.FS.ReadFile("003_episodic_memory.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	if _, err := db.Exec(`INSERT INTO episodic_events(session_id, seq, timestamp, event_type, source, content, salience, occurred_at, event_uuid)
		VALUES ('s1', 1, ?, 'observation', 'agent', 'x', 0.1, ?, 'uuid-1')`, old, old); err != nil {
		t.Fatal(err)
	}

	g := &recordingEdgeDeleter{}
	fm := NewForgettingManager(nil, nil, 0.01).WithGraphEdgeDeleter(g)
	if err := fm.cleanupWithSQL(context.Background(), db); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var archived int
	if err := db.QueryRow(`SELECT archived FROM episodic_events WHERE event_uuid='uuid-1'`).Scan(&archived); err != nil || archived != 1 {
		t.Fatalf("archived=%d err=%v, want 1 (tx must commit)", archived, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM episodic_events_change_log WHERE session_id='s1' AND change_type='archive'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("change_log rows=%d err=%v, want 1", n, err)
	}
	if len(g.from) != 1 || g.from[0] != "episodic:uuid-1" {
		t.Fatalf("graph edge cleanup = %v, want [episodic:uuid-1]", g.from)
	}
}
