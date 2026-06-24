package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type VerifyReport struct {
	Valid         bool
	CheckedCount  int
	FirstError    error
	ErrorOffset   int64
	LastValidHash string
}

type AuditChain struct {
	db *sql.DB
}

func NewAuditChain(db *sql.DB) *AuditChain {
	return &AuditChain{db: db}
}

func (a *AuditChain) VerifyChain(ctx context.Context, fromOffset int64) (VerifyReport, error) {
	report := VerifyReport{Valid: true}

	query := `SELECT offset, id, topic, actor, type, payload, prev_hash, hash 
	          FROM events WHERE offset >= ? ORDER BY offset ASC`
	rows, err := a.db.QueryContext(ctx, query, fromOffset)
	if err != nil {
		return report, apperr.Wrap(apperr.CodeInternal, "VerifyChain query failed", err)
	}
	defer rows.Close()

	var expectedPrevHash string
	isFirstRow := true

	for rows.Next() {
		var (
			offset      int64
			id          string
			topic       string
			actor       string
			evtType     string
			payload     []byte
			prevHash    sql.NullString
			currentHash string
		)
		if err := rows.Scan(&offset, &id, &topic, &actor, &evtType, &payload, &prevHash, &currentHash); err != nil {
			return report, apperr.Wrap(apperr.CodeInternal, "VerifyChain scan failed", err)
		}

		if !isFirstRow {
			if prevHash.String != expectedPrevHash {
				report.Valid = false
				report.FirstError = fmt.Errorf("hash chain broken at offset %d: expected prev_hash %q, got %q", offset, expectedPrevHash, prevHash.String)
				report.ErrorOffset = offset
				return report, nil
			}
		}
		isFirstRow = false

		h := sha256.New()
		h.Write([]byte(id))
		h.Write([]byte(topic))
		h.Write([]byte(actor))
		h.Write([]byte(evtType))
		h.Write(payload)
		if prevHash.Valid {
			h.Write([]byte(prevHash.String))
		}
		computedHash := hex.EncodeToString(h.Sum(nil))

		if computedHash != currentHash {
			report.Valid = false
			report.FirstError = fmt.Errorf("hash mismatch at offset %d: expected %q, got %q", offset, computedHash, currentHash)
			report.ErrorOffset = offset
			return report, nil
		}

		expectedPrevHash = currentHash
		report.LastValidHash = currentHash
		report.CheckedCount++
	}

	if err := rows.Err(); err != nil {
		return report, apperr.Wrap(apperr.CodeInternal, "VerifyChain rows iteration failed", err)
	}

	return report, nil
}
