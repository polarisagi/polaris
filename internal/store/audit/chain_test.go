package audit

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestVerifyChain_FromOffset(t *testing.T) {
	db, err := sql.Open("sqlite", "file:testchain?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE events (
			offset INTEGER PRIMARY KEY AUTOINCREMENT,
			id TEXT, topic TEXT, actor TEXT, type TEXT, payload BLOB, prev_hash TEXT, hash TEXT
		)
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	chain := NewAuditChain(db)

	// Create some events, we just need the hash/prev_hash to be consistent for the mock
	// We can insert some invalid hashes and ensure VerifyChain fails when checking fromOffset>0

	_, _ = db.Exec(`INSERT INTO events (offset, id, topic, actor, type, payload, prev_hash, hash) VALUES 
		(0, '1', 't', 'a', 'ty', X'00', NULL, 'hash0'),
		(1, '2', 't', 'a', 'ty', X'00', 'hash0', 'hash1'),
		(2, '3', 't', 'a', 'ty', X'00', 'badhash', 'hash2')
	`)

	// If fromOffset = 1, expectedPrevHash should be 'hash0', so offset 1 is valid (if hash calculation was bypassed, but it computes hash).
	// Let's just check the error offset.
	report, err := chain.VerifyChain(context.Background(), 2)
	if err != nil {
		t.Logf("err: %v", err)
	}
	if report.Valid {
		t.Errorf("expected chain to be invalid")
	}
	if report.ErrorOffset != 2 {
		t.Errorf("expected error at offset 2, got %d", report.ErrorOffset)
	}
}
