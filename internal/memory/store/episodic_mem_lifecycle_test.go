package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/types"
)

type salienceGetQueryStore struct{ *testutil.MockStore }

func (s salienceGetQueryStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	var one int
	if err := s.DB().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return nil, err
	}
	return s.MockStore.Get(ctx, key)
}

func TestScanHighSalience_ResolvesProjectAndTaintAfterClosingRows(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewMockStore()
	defer st.DB().Close()
	ep := NewEpisodicMem(salienceGetQueryStore{MockStore: st})
	if _, err := st.ExecContext(ctx, `DROP TABLE episodic_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExecContext(ctx, `CREATE TABLE episodic_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, seq INTEGER, timestamp INTEGER,
		event_type TEXT, source TEXT, content TEXT, salience REAL, occurred_at INTEGER,
		archived INTEGER, event_uuid TEXT
	)`); err != nil {
		t.Fatal(err)
	}

	putEvent := func(key string, event types.Event) {
		t.Helper()
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put(ctx, []byte("episodic:"+key), raw); err != nil {
			t.Fatal(err)
		}
	}
	insertRow := func(sessionID, eventUUID, content string) {
		t.Helper()
		_, err := st.ExecContext(ctx, `INSERT INTO episodic_events
			(session_id, seq, timestamp, event_type, source, content, salience, event_uuid, archived)
			VALUES (?, ?, 1, 'observation', 'agent', ?, 0.9, ?, 0)`, sessionID, 1, content, eventUUID)
		if err != nil {
			t.Fatal(err)
		}
	}

	putEvent("default-high", types.Event{ID: "default-high", Payload: []byte("default"), TaintLevel: types.TaintHigh})
	insertRow("s-default", "default-high", "default")
	putEvent("named", types.Event{ID: "named", ProjectID: "project-a", Payload: []byte("named"), TaintLevel: types.TaintMedium})
	insertRow("s-named", "named", "named")
	insertRow("s-missing", "missing", "missing")
	if err := st.Put(ctx, []byte("episodic:invalid"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	insertRow("s-invalid", "invalid", "invalid")
	putEvent("mismatch", types.Event{ID: "another-id", ProjectID: "project-a"})
	insertRow("s-mismatch", "mismatch", "mismatch")
	putEvent("bad-taint", types.Event{ID: "bad-taint", TaintLevel: types.TaintLevel(99)})
	insertRow("s-bad-taint", "bad-taint", "bad taint")
	insertRow("s-no-uuid", "", "no uuid")
	if _, err := st.ExecContext(ctx, `INSERT INTO episodic_events
		(session_id, seq, timestamp, content, salience, event_uuid, archived)
		VALUES ('s-null-uuid', 1, 1, 'null uuid', 0.9, NULL, 0)`); err != nil {
		t.Fatal(err)
	}

	got, err := ep.ScanHighSalience(ctx, 0, 0.7, 20)
	if err != nil {
		t.Fatalf("ScanHighSalience: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("scan must retain every row for high-water advancement: got %d", len(got))
	}
	if e := got[0]; !e.MetadataValid || e.ProjectID != types.DefaultProjectID || e.TaintLevel != types.TaintHigh {
		t.Fatalf("valid legacy default event metadata lost: %+v", e)
	}
	if e := got[1]; !e.MetadataValid || e.ProjectID != "project-a" || e.TaintLevel != types.TaintMedium {
		t.Fatalf("named event metadata lost: %+v", e)
	}
	for _, e := range got[2:] {
		if e.MetadataValid {
			t.Errorf("row %d with missing/invalid/mismatched metadata marked valid: %+v", e.ID, e)
		}
	}
}
