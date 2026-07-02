package consolidation

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// EventArchiver performs cold archiving of events older than EventlogWarmDays
// into a secondary attached SQLite database.
type EventArchiver struct {
	db        *sql.DB
	warmDays  int
	coldDBDir string
}

func NewEventArchiver(db *sql.DB, warmDays int, coldDBDir string) *EventArchiver {
	return &EventArchiver{
		db:        db,
		warmDays:  warmDays,
		coldDBDir: coldDBDir,
	}
}

// Archive runs the archiving process. It attaches the cold DB, creates the events table
// if it doesn't exist, moves old events, and then detaches the cold DB.
func (ea *EventArchiver) Archive(ctx context.Context) error {
	if err := os.MkdirAll(ea.coldDBDir, 0755); err != nil {
		return fmt.Errorf("create cold db dir: %w", err)
	}

	coldDBPath := filepath.Join(ea.coldDBDir, "events_archive.db")

	// 1. ATTACH DATABASE
	attachStmt := fmt.Sprintf("ATTACH DATABASE '%s' AS cold_archive", coldDBPath)
	if _, err := ea.db.ExecContext(ctx, attachStmt); err != nil {
		return fmt.Errorf("attach cold db: %w", err)
	}
	defer func() {
		_, _ = ea.db.ExecContext(context.Background(), "DETACH DATABASE cold_archive")
	}()

	// 2. Create table in cold DB (mirroring the main events table)
	// We keep the exact same schema to ensure compatibility.
	createTableStmt := `
CREATE TABLE IF NOT EXISTS cold_archive.events (
    offset            INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT NOT NULL UNIQUE,
    topic             TEXT NOT NULL,
    actor             TEXT NOT NULL,
    type              TEXT NOT NULL,
    payload           BLOB NOT NULL,
    idempotency_key   TEXT UNIQUE,
    embedding         BLOB,
    memory_layer      TEXT DEFAULT 'episodic',
    salience          REAL DEFAULT 0.5,
    occurred_at       INTEGER,
    durative_group_id TEXT,
    created_at        INTEGER NOT NULL,
    metadata          TEXT,
    prev_hash         TEXT,
    hash              TEXT
);
	`
	if _, err := ea.db.ExecContext(ctx, createTableStmt); err != nil {
		return fmt.Errorf("create cold archive table: %w", err)
	}

	// 3. Calculate cutoff time in milliseconds
	cutoffMs := time.Now().AddDate(0, 0, -ea.warmDays).UnixMilli()

	// 4. Move data within a transaction
	tx, err := ea.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback() // Safe to call after commit
	}()

	insertStmt := `
		INSERT INTO cold_archive.events (
			offset, id, topic, actor, type, payload, idempotency_key, embedding, 
			memory_layer, salience, occurred_at, durative_group_id, created_at, 
			metadata, prev_hash, hash
		)
		SELECT 
			offset, id, topic, actor, type, payload, idempotency_key, embedding, 
			memory_layer, salience, occurred_at, durative_group_id, created_at, 
			metadata, prev_hash, hash
		FROM main.events 
		WHERE created_at < ? AND NOT EXISTS (
			SELECT 1 FROM cold_archive.events c WHERE c.id = main.events.id
		)
	`
	if _, err := tx.ExecContext(ctx, insertStmt, cutoffMs); err != nil {
		return fmt.Errorf("insert into cold db: %w", err)
	}

	deleteStmt := `DELETE FROM main.events WHERE created_at < ?`
	res, err := tx.ExecContext(ctx, deleteStmt, cutoffMs)
	if err != nil {
		return fmt.Errorf("delete from main db: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected > 0 {
		slog.Info("polaris: event archiver moved events to cold storage", "count", rowsAffected, "cutoff_ms", cutoffMs)
	}

	return nil
}
