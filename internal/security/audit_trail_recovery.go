package security

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const auditRecoveryBatchSize = 100

//nolint:gocyclo // Recovery is a linear fail-closed validation pipeline; splitting would obscure ordering.
func (at *AuditTrail) recoverAuditTail() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	descRows, err := at.repo.ListAuditTail(ctx, auditRecoveryBatchSize, "")
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "audit: query events for recovery", err)
	}
	desc := make([]*AuditRecord, len(descRows))
	for i, row := range descRows {
		rec, err := decodeAuditRow(row)
		if err != nil {
			return err
		}
		desc[i] = rec
		if i > 0 && desc[i-1].PrevHash != rec.RecordHash {
			return auditRecoveryError(row.ID, i, "hash chain link does not match the preceding row")
		}
	}
	if err := validateRotationMarkers(desc); err != nil {
		return err
	}

	var anchor *AuditRecord
	if len(desc) > 0 && desc[len(desc)-1].PrevHash != "" { //nolint:nestif // the anchor must be validated before accepting the tail.
		anchorRows, err := at.repo.ListAuditTail(ctx, 1, descRows[len(descRows)-1].ID)
		if err != nil {
			return apperr.Wrap(apperr.CodeStorageUnavailable, "audit: query integrity anchor", err)
		}
		if len(anchorRows) != 1 {
			return auditRecoveryError(descRows[len(descRows)-1].ID, len(descRows)-1, "previous hash is non-empty but the preceding database row is missing")
		}
		anchor, err = decodeAuditRow(anchorRows[0])
		if err != nil {
			return err
		}
		if desc[len(desc)-1].PrevHash != anchor.RecordHash {
			return auditRecoveryError(descRows[len(descRows)-1].ID, len(descRows)-1, "tail anchor hash does not match")
		}
		if err := validateRotationMarkers([]*AuditRecord{desc[len(desc)-1], anchor}); err != nil {
			return err
		}
	}

	epochID, pendingStart, err := at.recoverEpochState(ctx, desc, descRows)
	if err != nil {
		return err
	}

	records := make([]*AuditRecord, len(desc))
	for i := range desc {
		records[len(desc)-1-i] = desc[i]
	}
	anchorHash := ""
	if anchor != nil {
		anchorHash = anchor.RecordHash
	}
	candidate := &AuditTrail{records: records, epochStartHash: anchorHash}
	if idx, why := candidate.verifyIntegrityLocked(); idx >= 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"audit: 审计链完整性校验失败，拒绝启动；断裂位置: 第 %d 条（共恢复 %d 条）；原因: %s",
			idx, len(records), why))
	}

	epochBytes := recoveredEpochBytes(desc, epochID, pendingStart != nil)
	at.mu.Lock()
	at.records = records
	at.epochStartHash = anchorHash
	at.epochID = epochID
	at.pendingEnd = nil
	at.pendingStart = pendingStart
	at.epochBytes = epochBytes
	if len(desc) > 0 {
		at.lastHash = desc[0].RecordHash
	} else {
		at.lastHash = ""
	}
	// end-only 恢复须先补写确定性 epoch_start；失败则启动失败并保留 pending 状态。
	err = at.finishPendingRotationLocked()
	at.mu.Unlock()
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "audit: resume recovered epoch rotation", err)
	}
	return nil
}

//nolint:gocyclo // Epoch recovery validates marker chronology and reconstructs state as one transaction.
func (at *AuditTrail) recoverEpochState(
	ctx context.Context,
	desc []*AuditRecord,
	rows []types.AuditEventRow,
) (int, *AuditRecord, error) {
	if len(desc) == 0 {
		return 0, nil, nil
	}
	if desc[0].EpochID != nil {
		return epochStateFromRecord(desc[0])
	}
	for _, rec := range desc {
		if rec.ActionType == "epoch_start" || rec.ActionType == "epoch_end" {
			return epochStateFromRecord(rec)
		}
	}
	if desc[len(desc)-1].PrevHash == "" {
		return 0, nil, nil
	}

	// 升级前的普通记录没有 EpochID；按物理追加游标向前找最近 marker，
	// 每批校验记录 ID/hash/链接，并受同一恢复 deadline 限制。
	expectedHash := desc[len(desc)-1].PrevHash
	beforeID := rows[len(rows)-1].ID
	for {
		olderRows, err := at.repo.ListAuditTail(ctx, auditRecoveryBatchSize, beforeID)
		if err != nil {
			return 0, nil, apperr.Wrap(apperr.CodeStorageUnavailable, "audit: scan legacy epoch markers", err)
		}
		if len(olderRows) == 0 {
			return 0, nil, auditRecoveryError(beforeID, 0, "legacy chain ended before the expected previous hash")
		}
		for i, row := range olderRows {
			rec, err := decodeAuditRow(row)
			if err != nil {
				return 0, nil, err
			}
			if rec.RecordHash != expectedHash {
				return 0, nil, auditRecoveryError(row.ID, i, "legacy hash chain link does not match")
			}
			expectedHash = rec.PrevHash
			if rec.ActionType == "epoch_end" {
				return 0, nil, auditRecoveryError(row.ID, i, "legacy epoch_end has newer records but no intervening epoch_start")
			}
			if rec.ActionType == "epoch_start" || rec.EpochID != nil {
				return epochStateFromRecord(rec)
			}
			if expectedHash == "" {
				return 0, nil, nil
			}
		}
		beforeID = olderRows[len(olderRows)-1].ID
	}
}

func epochStateFromRecord(rec *AuditRecord) (int, *AuditRecord, error) {
	epochID, err := recordEpochID(rec)
	if err != nil {
		return 0, nil, err
	}
	if rec.ActionType == "epoch_end" {
		nextEpoch := epochID + 1
		return nextEpoch, newEpochStart(nextEpoch, rec.RecordHash, rec.Timestamp), nil
	}
	return epochID, nil, nil
}

//nolint:gocyclo // Backward-compatible epoch decoding has distinct legacy encodings to reject explicitly.
func recordEpochID(rec *AuditRecord) (int, error) {
	if rec.EpochID != nil { //nolint:nestif // explicit epoch is authoritative and checked against legacy metadata.
		if *rec.EpochID < 0 {
			return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: record %q has a negative epoch ID", rec.EventID))
		}
		if rec.ActionType == "epoch_start" || rec.ActionType == "epoch_end" {
			var details struct {
				EpochID            *int   `json:"epoch_id"`
				PrevEpochFinalHash string `json:"prev_epoch_final_hash"`
				FinalHash          string `json:"final_hash"`
			}
			if err := json.Unmarshal(rec.ActionDetail, &details); err != nil {
				return 0, apperr.Wrap(apperr.CodeInternal, "audit: decode epoch marker details", err)
			}
			if details.EpochID == nil || *details.EpochID != *rec.EpochID {
				return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch marker %q has inconsistent epoch IDs", rec.EventID))
			}
			if rec.ActionType == "epoch_start" && details.PrevEpochFinalHash != rec.PrevHash {
				return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch_start %q disagrees with its previous hash", rec.EventID))
			}
			if rec.ActionType == "epoch_end" && details.FinalHash != rec.PrevHash {
				return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch_end %q disagrees with its final hash", rec.EventID))
			}
		}
		if rec.ActionType == "epoch_start" && rec.EventID != fmt.Sprintf("epoch_start_%d", *rec.EpochID) {
			return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch_start %q has inconsistent marker ID", rec.EventID))
		}
		if rec.ActionType == "epoch_end" && rec.EventID != fmt.Sprintf("epoch_end_%d", *rec.EpochID) {
			return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch_end %q has inconsistent marker ID", rec.EventID))
		}
		return *rec.EpochID, nil
	}
	var details struct {
		EpochID            *int   `json:"epoch_id"`
		PrevEpochFinalHash string `json:"prev_epoch_final_hash"`
		FinalHash          string `json:"final_hash"`
	}
	if err := json.Unmarshal(rec.ActionDetail, &details); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "audit: decode epoch marker details", err)
	}
	if details.EpochID == nil {
		return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch marker %q has no epoch_id", rec.EventID))
	}
	if *details.EpochID < 0 {
		return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch marker %q has a negative epoch ID", rec.EventID))
	}
	if rec.ActionType == "epoch_start" {
		if rec.EventID != fmt.Sprintf("epoch_start_%d", *details.EpochID) || details.PrevEpochFinalHash != rec.PrevHash {
			return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch_start %q has inconsistent marker metadata", rec.EventID))
		}
	}
	if rec.ActionType == "epoch_end" {
		if rec.EventID != fmt.Sprintf("epoch_end_%d", *details.EpochID) || details.FinalHash != rec.PrevHash {
			return 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: epoch_end %q has inconsistent marker metadata", rec.EventID))
		}
	}
	return *details.EpochID, nil
}

func decodeAuditRow(row types.AuditEventRow) (*AuditRecord, error) {
	var rec AuditRecord
	if err := json.Unmarshal([]byte(row.Meta), &rec); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("audit: malformed persisted record at event %q", row.ID), err)
	}
	if rec.EventID != row.ID {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("audit: database row ID %q does not match payload EventID %q", row.ID, rec.EventID))
	}
	if !recordHashMatches(&rec) {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("audit: persisted record hash is invalid at event %q", row.ID))
	}
	return &rec, nil
}

//nolint:gocyclo // Marker pairs must be checked together for sequence and hash-chain invariants.
func validateRotationMarkers(desc []*AuditRecord) error {
	for i := 0; i+1 < len(desc); i++ {
		newer, older := desc[i], desc[i+1]
		if newer.EpochID == nil || older.EpochID == nil {
			continue
		}
		if newer.ActionType == "epoch_start" && older.ActionType == "epoch_end" {
			if *newer.EpochID != *older.EpochID+1 {
				return auditRecoveryError(newer.EventID, i, "epoch_start ID does not follow the preceding epoch_end")
			}
			continue
		}
		if *newer.EpochID != *older.EpochID {
			return auditRecoveryError(newer.EventID, i, "adjacent records disagree on epoch ID")
		}
	}
	for i, rec := range desc {
		switch rec.ActionType {
		case "epoch_end":
			endEpoch, err := recordEpochID(rec)
			if err != nil {
				return err
			}
			if i > 0 {
				if desc[i-1].ActionType != "epoch_start" {
					return auditRecoveryError(rec.EventID, i, "epoch_end is not immediately followed by epoch_start")
				}
				startEpoch, err := recordEpochID(desc[i-1])
				if err != nil {
					return err
				}
				if startEpoch != endEpoch+1 {
					return auditRecoveryError(rec.EventID, i, "epoch_start ID does not follow epoch_end ID")
				}
			}
		case "epoch_start":
			startEpoch, err := recordEpochID(rec)
			if err != nil {
				return err
			}
			if i+1 < len(desc) {
				if desc[i+1].ActionType != "epoch_end" {
					return auditRecoveryError(rec.EventID, i, "epoch_start is not linked to the preceding epoch_end")
				}
				endEpoch, err := recordEpochID(desc[i+1])
				if err != nil {
					return err
				}
				if startEpoch != endEpoch+1 {
					return auditRecoveryError(rec.EventID, i, "epoch_start ID does not follow epoch_end ID")
				}
			}
		}
	}
	return nil
}

func recoveredEpochBytes(desc []*AuditRecord, epochID int, pendingStart bool) int64 {
	if len(desc) == 0 || pendingStart {
		return 0
	}
	var total int64
	for _, rec := range desc {
		if rec.EpochID != nil && *rec.EpochID != epochID {
			break
		}
		total += int64(len(serializeRecord(rec)))
		if rec.ActionType == "epoch_start" {
			break
		}
	}
	return total
}

func auditRecoveryError(eventID string, index int, reason string) error {
	return apperr.New(apperr.CodeInternal,
		fmt.Sprintf("audit: recovery integrity failure at event %q row %d: %s", eventID, index, reason))
}
