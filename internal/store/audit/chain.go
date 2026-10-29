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

//nolint:gocyclo
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

	// GR-1-003: fromOffset>0 时，先查前一行 hash 作为链首校验锚点，
	// 防止归档/截断后的链首断裂被静默跳过
	if fromOffset > 0 {
		var prevRowHash sql.NullString
		anchorQuery := `SELECT hash FROM events WHERE offset = ?`
		if err := a.db.QueryRowContext(ctx, anchorQuery, fromOffset-1).Scan(&prevRowHash); err != nil && err != sql.ErrNoRows {
			return report, apperr.Wrap(apperr.CodeInternal, "VerifyChain: query anchor hash failed", err)
		}
		if prevRowHash.Valid {
			expectedPrevHash = prevRowHash.String
		}
		isFirstRow = false // 取消 isFirstRow 跳过逻辑，首行纳入校验
	}

	for rows.Next() {
		var (
			offset      int64
			id          string
			topic       string
			actor       string
			evtType     string
			payload     []byte
			prevHash    sql.NullString
			currentHash sql.NullString
		)
		if err := rows.Scan(&offset, &id, &topic, &actor, &evtType, &payload, &prevHash, &currentHash); err != nil {
			return report, apperr.Wrap(apperr.CodeInternal, "VerifyChain scan failed", err)
		}

		if isFirstRow && fromOffset == 0 && prevHash.Valid && prevHash.String != "" {
			// 链头 prev_hash 非空说明已被归档截断，应视为校验失败（GR-5-001）
			report.Valid = false
			report.FirstError = apperr.New(apperr.CodeInternal,
				fmt.Sprintf("audit chain truncated at offset %d: first row has non-null prev_hash %q, events may have been archived",
					offset, prevHash.String))
			report.ErrorOffset = offset
			return report, nil
		}

		if !isFirstRow {
			if prevHash.String != expectedPrevHash {
				report.Valid = false
				report.FirstError = apperr.New(apperr.CodeInternal, fmt.Sprintf("hash chain broken at offset %d: expected prev_hash %q, got %q", offset, expectedPrevHash, prevHash.String))
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

		if currentHash.Valid && computedHash != currentHash.String {
			report.Valid = false
			report.FirstError = apperr.New(apperr.CodeInternal, fmt.Sprintf("hash mismatch at offset %d: expected %q, got %q", offset, computedHash, currentHash.String))
			report.ErrorOffset = offset
			return report, nil
		}

		expectedPrevHash = currentHash.String
		report.LastValidHash = currentHash.String
		report.CheckedCount++
	}

	if err := rows.Err(); err != nil {
		return report, apperr.Wrap(apperr.CodeInternal, "VerifyChain rows iteration failed", err)
	}

	return report, nil
}
