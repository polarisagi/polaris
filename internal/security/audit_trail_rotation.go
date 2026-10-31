package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func epochIDPointer(epochID int) *int { return &epochID }

func newEpochEnd(epochID int, finalHash string, recordCount int) *AuditRecord {
	end := &AuditRecord{
		EventID:    fmt.Sprintf("epoch_end_%d", epochID),
		Timestamp:  time.Now().UnixMicro(),
		ActionType: "epoch_end",
		ActionDetail: mustJSON(map[string]any{
			"epoch_id":     epochID,
			"record_count": recordCount,
			"final_hash":   finalHash,
		}),
		PrevHash: finalHash,
		EpochID:  epochIDPointer(epochID),
	}
	setRecordHash(end)
	return end
}

func newEpochStart(epochID int, prevEpochFinalHash string, timestamp int64) *AuditRecord {
	start := &AuditRecord{
		EventID:    fmt.Sprintf("epoch_start_%d", epochID),
		Timestamp:  timestamp,
		ActionType: "epoch_start",
		ActionDetail: mustJSON(map[string]any{
			"epoch_id":              epochID,
			"prev_epoch_final_hash": prevEpochFinalHash,
		}),
		PrevHash: prevEpochFinalHash,
		EpochID:  epochIDPointer(epochID),
	}
	setRecordHash(start)
	return start
}

func setRecordHash(record *AuditRecord) {
	sum := sha256.Sum256(serializeRecord(record))
	record.RecordHash = hex.EncodeToString(sum[:])
}

// persistRecordLocked 处理 Append 返回错误但底层可能已提交的歧义结果。
// exact ID + Meta 读回证明同一条记录已持久化，其他情况保持 fail-closed。
func (at *AuditTrail) persistRecordLocked(record *AuditRecord) error {
	if at.repo == nil {
		return nil
	}
	actor := record.AgentID
	if actor == "" {
		actor = "system"
	}
	row := types.AuditEventRow{
		ID:     record.EventID,
		Actor:  actor,
		Action: "system",
		Meta:   string(mustJSON(record)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := at.repo.AppendAuditEvent(ctx, row)
	cancel()
	if err == nil {
		return nil
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	stored, readErr := at.repo.GetAuditEvent(readCtx, row.ID)
	readCancel()
	if readErr == nil && stored != nil {
		if stored.ID == row.ID && stored.Meta == row.Meta && stored.Action == row.Action && stored.Actor == row.Actor {
			return nil
		}
		return apperr.New(apperr.CodeConflict, fmt.Sprintf("audit: event ID %q exists with different persisted content", row.ID))
	}
	if readErr != nil && !apperr.IsCode(readErr, apperr.CodeNotFound) {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "audit: append outcome and exact readback are unavailable", errors.Join(err, readErr))
	}
	return apperr.Wrap(apperr.CodeStorageUnavailable, "audit: event append was not confirmed", err)
}

// finishPendingRotationLocked 仅在每个 marker 的持久化结果得到确认后推进内存状态。
func (at *AuditTrail) finishPendingRotationLocked() error {
	if err := at.finishPendingRecordLocked(); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "audit: persist pending record", err)
	}
	if at.pendingEnd != nil {
		end := at.pendingEnd
		if err := at.persistRecordLocked(end); err != nil {
			return apperr.Wrap(apperr.CodeOf(err), "audit: persist epoch_end", err)
		}
		at.records = append(at.records, end)
		at.lastHash = end.RecordHash
		at.epochBytes = 0
		at.epochID++
		at.pendingEnd = nil
		at.pendingStart = newEpochStart(at.epochID, end.RecordHash, end.Timestamp)
	}

	if at.pendingStart != nil {
		start := at.pendingStart
		if err := at.persistRecordLocked(start); err != nil {
			return apperr.Wrap(apperr.CodeOf(err), "audit: persist epoch_start", err)
		}
		at.records = []*AuditRecord{start}
		at.lastHash = start.RecordHash
		at.epochStartHash = start.PrevHash
		at.epochBytes = int64(len(serializeRecord(start)))
		at.pendingStart = nil
	}
	return nil
}

func (at *AuditTrail) finishPendingRecordLocked() error {
	if at.pendingRecord == nil {
		return nil
	}
	record := at.pendingRecord
	if err := at.persistRecordLocked(record); err != nil {
		return err
	}
	at.records = append(at.records, record)
	at.lastHash = record.RecordHash
	at.epochBytes += int64(len(serializeRecord(record)))
	at.pendingRecord = nil
	at.pendingRecordCaller = nil
	return nil
}
