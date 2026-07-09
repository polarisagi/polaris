package security

import (
	"context"
	"database/sql"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/internal/store/repo"
)

type mockEventLogger struct{ db *sql.DB }

func (m *mockEventLogger) AppendEvent(ctx context.Context, ev *pb.Event) error {
	_, err := m.db.ExecContext(ctx, "INSERT INTO events (id, topic, actor, type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)", ev.Id, ev.Topic, ev.Actor, ev.Type, ev.Payload, ev.CreatedAt)
	return err
}

func TestAuditTrail_Record_WritesToDB(t *testing.T) {
	db := openTestDB(t)
	at := NewAuditTrail(repo.NewSQLiteAuditRepository(db, &mockEventLogger{db: db}), t.TempDir())

	err := at.Record(&AuditRecord{
		ActionType: "test_action",
	})
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM events WHERE topic = 'audit.policy'").Scan(&count)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 record in db, got %d", count)
	}
}

func TestAuditTrail_VerifyIntegrity_CleanChain(t *testing.T) {
	at := NewAuditTrail(nil, t.TempDir())
	at.Record(&AuditRecord{ActionType: "a1"})
	at.Record(&AuditRecord{ActionType: "a2"})

	ok, idx := at.VerifyIntegrity()
	if !ok {
		t.Errorf("expected clean chain, failed at %d", idx)
	}
}

func TestAuditTrail_VerifyIntegrity_TamperedRecord(t *testing.T) {
	at := NewAuditTrail(nil, t.TempDir())
	at.Record(&AuditRecord{ActionType: "a1"})
	at.Record(&AuditRecord{ActionType: "a2"})

	// Tamper with record
	at.records[0].ActionType = "tampered"

	ok, idx := at.VerifyIntegrity()
	if ok {
		t.Error("expected integrity check to fail")
	}
	if idx != 0 {
		t.Errorf("expected failure at index 0, got %d", idx)
	}
}

func TestAuditTrail_RotateIfNeeded_BelowThreshold_NoRotate(t *testing.T) {
	at := NewAuditTrail(nil, t.TempDir())
	at.Record(&AuditRecord{ActionType: "a1"})

	err := at.RotateIfNeeded(50) // limit is 100
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if at.epochID != 0 {
		t.Errorf("expected epoch 0, got %d", at.epochID)
	}
}

func TestAuditTrail_RotateIfNeeded_AboveThreshold_Rotates(t *testing.T) {
	db := openTestDB(t)
	at := NewAuditTrail(repo.NewSQLiteAuditRepository(db, &mockEventLogger{db: db}), t.TempDir())
	at.Record(&AuditRecord{ActionType: "a1"})

	err := at.RotateIfNeeded(150)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if at.epochID != 1 {
		t.Errorf("expected epoch 1, got %d", at.epochID)
	}
	if len(at.records) != 1 || at.records[0].ActionType != "epoch_start" {
		t.Errorf("expected new epoch to start with epoch_start, got %v", at.records)
	}
}

func TestAuditTrail_RecoverOnStartup_ReloadsFromDB(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	// Create some records
	at1 := NewAuditTrail(repo.NewSQLiteAuditRepository(db, &mockEventLogger{db: db}), dir)
	at1.Record(&AuditRecord{EventID: "evt_1", ActionType: "a1"})
	at1.Record(&AuditRecord{EventID: "evt_2", ActionType: "a2"})

	// Create a new AuditTrail to simulate startup
	at2 := NewAuditTrail(repo.NewSQLiteAuditRepository(db, &mockEventLogger{db: db}), dir)
	err := at2.RecoverOnStartup()
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	if len(at2.records) != 2 {
		t.Errorf("expected 2 records, got %d", len(at2.records))
	}
	if len(at2.records) == 2 && (at2.records[0].ActionType != "a1" || at2.records[1].ActionType != "a2") {
		t.Errorf("records did not match")
	}
}

func TestSerializeRecord_Deterministic(t *testing.T) {
	r1 := &AuditRecord{ActionType: "a1", TrustLevel: 5}
	r2 := &AuditRecord{ActionType: "a1", TrustLevel: 5}

	s1 := serializeRecord(r1)
	s2 := serializeRecord(r2)

	if string(s1) != string(s2) {
		t.Errorf("expected deterministic serialization")
	}
}
