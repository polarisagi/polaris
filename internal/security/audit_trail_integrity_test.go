package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type controlledAuditRepository struct {
	rows           []types.AuditEventRow
	failBefore     map[string]int
	commitThenFail map[string]int
	listTailFailAt string
	getReadError   error
	delayedCommit  map[string]types.AuditEventRow
	getNotFound    map[string]int
	appendAttempts []types.AuditEventRow
	getCalls       int
	listTailCalls  int
}

func newControlledAuditRepository() *controlledAuditRepository {
	return &controlledAuditRepository{
		failBefore:     make(map[string]int),
		commitThenFail: make(map[string]int),
		delayedCommit:  make(map[string]types.AuditEventRow),
		getNotFound:    make(map[string]int),
	}
}

func (r *controlledAuditRepository) AppendAuditEvent(_ context.Context, row types.AuditEventRow) error {
	r.appendAttempts = append(r.appendAttempts, row)
	if r.failBefore[row.ID] > 0 {
		r.failBefore[row.ID]--
		return apperr.New(apperr.CodeStorageUnavailable, "injected append failure")
	}
	for _, old := range r.rows {
		if old.ID == row.ID {
			return apperr.New(apperr.CodeAlreadyExists, "duplicate event ID")
		}
	}
	r.rows = append(r.rows, row)
	if r.commitThenFail[row.ID] > 0 {
		r.commitThenFail[row.ID]--
		return apperr.New(apperr.CodeTimeout, "injected post-commit timeout")
	}
	return nil
}

func (r *controlledAuditRepository) ListAuditEvents(_ context.Context, limit int, _ string) ([]types.AuditEventRow, error) {
	return r.ListAuditTail(context.Background(), limit, "")
}

func (r *controlledAuditRepository) ListAuditTail(_ context.Context, limit int, beforeID string) ([]types.AuditEventRow, error) {
	r.listTailCalls++
	if r.listTailFailAt != "" && r.listTailFailAt == beforeID {
		return nil, apperr.New(apperr.CodeStorageUnavailable, "injected tail read failure")
	}
	end := len(r.rows)
	if beforeID != "" {
		found := false
		for i := range r.rows {
			if r.rows[i].ID == beforeID {
				end = i
				found = true
				break
			}
		}
		if !found {
			return nil, apperr.New(apperr.CodeNotFound, "cursor not found")
		}
	}
	start := max(0, end-limit)
	result := make([]types.AuditEventRow, 0, end-start)
	for i := end - 1; i >= start; i-- {
		result = append(result, r.rows[i])
	}
	return result, nil
}

func (r *controlledAuditRepository) GetAuditEvent(_ context.Context, id string) (*types.AuditEventRow, error) {
	r.getCalls++
	if r.getReadError != nil {
		return nil, r.getReadError
	}
	for _, row := range r.rows {
		if row.ID == id {
			copy := row
			return &copy, nil
		}
	}
	if r.getNotFound[id] > 0 {
		r.getNotFound[id]--
		return nil, apperr.New(apperr.CodeNotFound, "event not found")
	}
	if delayed, ok := r.delayedCommit[id]; ok {
		r.rows = append(r.rows, delayed)
		delete(r.delayedCommit, id)
		copy := delayed
		return &copy, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, "event not found")
}

func (*controlledAuditRepository) DeleteAuditEventsBefore(context.Context, string) (int64, error) {
	return 0, nil
}

func (r *controlledAuditRepository) appendRecord(t *testing.T, record *AuditRecord) {
	t.Helper()
	if err := r.AppendAuditEvent(context.Background(), auditRow(record)); err != nil {
		t.Fatal(err)
	}
}

func auditRow(record *AuditRecord) types.AuditEventRow {
	actor := record.AgentID
	if actor == "" {
		actor = "system"
	}
	return types.AuditEventRow{ID: record.EventID, Actor: actor, Action: "system", Meta: string(mustJSON(record))}
}

func validAuditRecord(eventID, action string, prevHash string, epoch *int) *AuditRecord {
	record := &AuditRecord{
		EventID:    eventID,
		Timestamp:  1_780_000_000_000_000,
		ActionType: action,
		PrevHash:   prevHash,
		EpochID:    epoch,
	}
	if action == "epoch_start" || action == "epoch_end" {
		id := 0
		if epoch != nil {
			id = *epoch
		}
		if action == "epoch_end" {
			record.ActionDetail = mustJSON(map[string]any{"epoch_id": id, "record_count": 1, "final_hash": prevHash})
		} else {
			record.ActionDetail = mustJSON(map[string]any{"epoch_id": id, "prev_epoch_final_hash": prevHash})
		}
	}
	setRecordHash(record)
	return record
}

func TestRecoverOnStartupRejectsMalformedRowIDMismatchAndInvalidHash(t *testing.T) {
	tests := []struct {
		name string
		row  types.AuditEventRow
	}{
		{name: "malformed tail json", row: types.AuditEventRow{ID: "bad-json", Meta: "{"}},
		{name: "row id mismatch", row: func() types.AuditEventRow {
			row := auditRow(validAuditRecord("payload-id", "action", "", nil))
			row.ID = "database-id"
			return row
		}()},
		{name: "invalid hash", row: types.AuditEventRow{ID: "bad-hash", Meta: func() string {
			record := validAuditRecord("bad-hash", "action", "", nil)
			record.RecordHash = strings.Repeat("0", 64)
			return string(mustJSON(record))
		}()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newControlledAuditRepository()
			repo.rows = []types.AuditEventRow{tt.row}
			at := NewAuditTrail(repo, "")
			if err := at.RecoverOnStartup(); err == nil {
				t.Fatal("corrupt persisted row must fail recovery")
			}
			if len(at.records) != 0 {
				t.Fatalf("failed recovery must not install partial state: %+v", at.records)
			}
		})
	}
}

func TestRecoverOnStartupFailsClosedOnBadAnchor(t *testing.T) {
	t.Run("query error", func(t *testing.T) {
		repo := newControlledAuditRepository()
		appendIntegrityChain(t, repo, 101)
		repo.listTailFailAt = repo.rows[1].ID
		if err := NewAuditTrail(repo, "").RecoverOnStartup(); err == nil {
			t.Fatal("anchor query error must fail recovery")
		}
	})

	t.Run("malformed metadata", func(t *testing.T) {
		repo := newControlledAuditRepository()
		appendIntegrityChain(t, repo, 101)
		repo.rows[0].Meta = "{" // corrupt the only row that can anchor the recovered tail
		if err := NewAuditTrail(repo, "").RecoverOnStartup(); err == nil {
			t.Fatal("malformed anchor must fail recovery")
		}
	})
}

func appendIntegrityChain(t *testing.T, repo *controlledAuditRepository, n int) {
	t.Helper()
	prev := ""
	for i := range n {
		record := validAuditRecord(fmt.Sprintf("chain-%d", i), "action", prev, nil)
		repo.appendRecord(t, record)
		prev = record.RecordHash
	}
}

func TestAuditTrail_RecordPendingAppendBlocksAndRetriesSameID(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	record := &AuditRecord{EventID: "late-commit", ActionType: "action", ActionDetail: []byte("stable"), DataSubjects: []string{"user"}}
	repo.failBefore[record.EventID] = 1
	repo.getNotFound[record.EventID] = 1
	if err := at.Record(record); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("unconfirmed append must return storage unavailable, got %v", err)
	}
	if at.lastHash != "" || len(at.records) != 0 || at.pendingRecord == nil {
		t.Fatalf("unconfirmed append must stay pending without advancing memory chain: hash=%q records=%d pending=%v", at.lastHash, len(at.records), at.pendingRecord)
	}
	stableMeta := string(mustJSON(at.pendingRecord))
	// 原调用方后续改动不能改写待重试载荷。
	record.ActionDetail[0] = 'X'
	record.DataSubjects[0] = "changed"
	if err := at.Record(&AuditRecord{EventID: "next", ActionType: "action"}); err != nil {
		t.Fatalf("new append should proceed only after exact readback confirms the pending row, got %v", err)
	}
	if len(repo.rows) != 2 || repo.rows[0].ID != "late-commit" || repo.rows[1].ID != "next" {
		t.Fatalf("exact readback must confirm delayed commit before later event, rows=%+v", repo.rows)
	}
	var persisted AuditRecord
	if err := json.Unmarshal([]byte(repo.rows[0].Meta), &persisted); err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(&persisted)) != stableMeta {
		t.Fatalf("pending payload changed after caller mutation: got %s want %s", mustJSON(&persisted), stableMeta)
	}
	if at.pendingRecord != nil || len(at.records) != 2 || at.records[0].RecordHash != persisted.RecordHash || at.records[1].PrevHash != persisted.RecordHash {
		t.Fatalf("confirmed pending record must advance exactly once: pending=%v records=%d hash=%q", at.pendingRecord, len(at.records), at.lastHash)
	}

	t.Run("same caller retry is idempotent while pending", func(t *testing.T) {
		retryRepo := newControlledAuditRepository()
		retryTrail := NewAuditTrail(retryRepo, "")
		retryRecord := &AuditRecord{EventID: "retry-same", ActionType: "action"}
		retryRepo.failBefore[retryRecord.EventID] = 2
		retryRepo.getNotFound[retryRecord.EventID] = 2
		if err := retryTrail.Record(retryRecord); err == nil {
			t.Fatal("first uncertain append should fail")
		}
		if err := retryTrail.Record(retryRecord); err == nil {
			t.Fatal("retry with unavailable confirmation should remain pending")
		}
		if len(retryTrail.records) != 0 {
			t.Fatal("retry must not advance before persistence confirmation")
		}
		if err := retryTrail.Record(retryRecord); err != nil {
			t.Fatalf("same caller retry after successful persistence should be idempotent, got %v", err)
		}
		if len(retryRepo.rows) != 1 || len(retryTrail.records) != 1 {
			t.Fatalf("retry must append once, rows=%d records=%d", len(retryRepo.rows), len(retryTrail.records))
		}
	})
}

func TestAuditTrail_RecordRejectsNil(t *testing.T) {
	if err := NewAuditTrail(newControlledAuditRepository(), "").Record(nil); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("nil audit record must fail with typed invalid input, got %v", err)
	}
}

func TestAuditTrail_RotateConfirmsPendingRecordBeforeCreatingEndMarker(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	if err := at.Record(&AuditRecord{EventID: "before-pending", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	pending := &AuditRecord{EventID: "late-before-rotate", ActionType: "action"}
	repo.failBefore[pending.EventID] = 1
	repo.getNotFound[pending.EventID] = 1
	if err := at.Record(pending); err == nil {
		t.Fatal("uncertain append should remain pending")
	}
	// 复刻底层Append超时后提交，但精确读回首次尚未可见的歧义。
	repo.rows = append(repo.rows, auditRow(at.pendingRecord))
	if err := at.RotateIfNeeded(epochSizeLimitMB); err != nil {
		t.Fatalf("rotation should confirm pending record before markers: %v", err)
	}
	if len(repo.rows) != 4 || repo.rows[1].ID != pending.EventID || repo.rows[2].ID != "epoch_end_0" || repo.rows[3].ID != "epoch_start_1" {
		t.Fatalf("persist order must include pending event before rotation markers: %+v", repo.rows)
	}
	var persistedPending AuditRecord
	if err := json.Unmarshal([]byte(repo.rows[1].Meta), &persistedPending); err != nil {
		t.Fatal(err)
	}
	var end AuditRecord
	if err := json.Unmarshal([]byte(repo.rows[2].Meta), &end); err != nil {
		t.Fatal(err)
	}
	if end.PrevHash != persistedPending.RecordHash {
		t.Fatalf("epoch_end must chain from the newly confirmed pending event: end prev=%s pending hash=%s", end.PrevHash, persistedPending.RecordHash)
	}
	if ok, idx := at.VerifyIntegrity(); !ok {
		t.Fatalf("in-memory chain after rotation is broken at %d", idx)
	}
	recovered := NewAuditTrail(repo, "")
	if err := recovered.RecoverOnStartup(); err != nil {
		t.Fatalf("persisted chain after pending rotation must recover: %v", err)
	}
}

func TestRotateIfNeededEndFailureIsRetryableBeforeNextRecord(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	if err := at.Record(&AuditRecord{EventID: "before", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	lastHash := at.lastHash
	repo.failBefore["epoch_end_0"] = 2
	if err := at.RotateIfNeeded(epochSizeLimitMB); err == nil {
		t.Fatal("explicit rotation must surface marker persistence failure")
	}
	if at.epochID != 0 || at.lastHash != lastHash || at.pendingEnd == nil {
		t.Fatalf("failed end must retain old epoch and pending marker: epoch=%d hash=%s pending=%v", at.epochID, at.lastHash, at.pendingEnd)
	}
	if err := at.Record(&AuditRecord{EventID: "blocked", ActionType: "action"}); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("new record must be refused while pending marker fails, got %v", err)
	}
	if err := at.Record(&AuditRecord{EventID: "after-retry", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	if got := []string{repo.rows[1].ID, repo.rows[2].ID, repo.rows[3].ID}; fmt.Sprint(got) != "[epoch_end_0 epoch_start_1 after-retry]" {
		t.Fatalf("retry must persist markers before the next action: %v", got)
	}
	if ok, idx := at.VerifyIntegrity(); !ok {
		t.Fatalf("chain should remain valid after retry, broken at %d", idx)
	}
}

func TestRotateIfNeededStartFailureAndEndOnlyRecovery(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	if err := at.Record(&AuditRecord{EventID: "before", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	repo.failBefore["epoch_start_1"] = 1
	if err := at.RotateIfNeeded(epochSizeLimitMB); err == nil {
		t.Fatal("start failure must be returned by explicit rotation")
	}
	if at.epochID != 1 || at.pendingStart == nil || len(repo.rows) != 2 {
		t.Fatalf("end should be committed while start remains pending: epoch=%d pending=%v rows=%d", at.epochID, at.pendingStart, len(repo.rows))
	}
	expectedMeta := string(mustJSON(at.pendingStart))

	recovered := NewAuditTrail(repo, "")
	if err := recovered.RecoverOnStartup(); err != nil {
		t.Fatal(err)
	}
	if len(repo.rows) != 3 || repo.rows[2].ID != "epoch_start_1" || repo.rows[2].Meta != expectedMeta {
		t.Fatalf("end-only recovery must write the same deterministic start marker: %+v", repo.rows)
	}
	if ok, idx := recovered.VerifyIntegrity(); !ok {
		t.Fatalf("recovered chain should verify, broken at %d", idx)
	}
}

func TestRotateIfNeededExactReadbackAfterCommitTimeout(t *testing.T) {
	repo := newControlledAuditRepository()
	repo.commitThenFail["epoch_end_0"] = 1
	at := NewAuditTrail(repo, "")
	if err := at.Record(&AuditRecord{EventID: "before", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	if err := at.RotateIfNeeded(epochSizeLimitMB); err != nil {
		t.Fatalf("exact readback should confirm ambiguous commit: %v", err)
	}
	if repo.getCalls != 1 || len(repo.rows) != 3 || repo.rows[1].ID != "epoch_end_0" || repo.rows[2].ID != "epoch_start_1" {
		t.Fatalf("committed marker must be reconciled without duplicate: reads=%d rows=%+v", repo.getCalls, repo.rows)
	}
}

func TestRotateIfNeededRejectsReadbackWithDifferentMeta(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	if err := at.Record(&AuditRecord{EventID: "before", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	conflicting := validAuditRecord("epoch_end_0", "different-marker", at.lastHash, nil)
	repo.appendRecord(t, conflicting)
	lastHash := at.lastHash
	if err := at.RotateIfNeeded(epochSizeLimitMB); !apperr.IsCode(err, apperr.CodeConflict) {
		t.Fatalf("same ID with different Meta must be a hard conflict, got %v", err)
	}
	if at.epochID != 0 || at.lastHash != lastHash || at.pendingEnd == nil {
		t.Fatalf("conflicting marker must not advance memory state: epoch=%d hash=%s pending=%v", at.epochID, at.lastHash, at.pendingEnd)
	}
}

func TestStartFailureIsRepairedBeforeNextRecord(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	if err := at.Record(&AuditRecord{EventID: "before", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	repo.failBefore["epoch_start_1"] = 1
	if err := at.RotateIfNeeded(epochSizeLimitMB); err == nil {
		t.Fatal("rotation should report failed epoch_start")
	}
	if err := at.Record(&AuditRecord{EventID: "after", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	if got := []string{repo.rows[1].ID, repo.rows[2].ID, repo.rows[3].ID}; fmt.Sprint(got) != "[epoch_end_0 epoch_start_1 after]" {
		t.Fatalf("normal record must not precede pending epoch_start: %v", got)
	}
	if ok, idx := at.VerifyIntegrity(); !ok {
		t.Fatalf("chain should verify after pending start retry, broken at %d", idx)
	}
}

func TestRecordDoesNotReportDurableActionAsFailedWhenAutoRotationFails(t *testing.T) {
	repo := newControlledAuditRepository()
	at := NewAuditTrail(repo, "")
	at.epochBytes = epochSizeLimitMB<<20 - 1
	repo.failBefore["epoch_end_0"] = 1
	if err := at.Record(&AuditRecord{EventID: "durable-action", ActionType: "action"}); err != nil {
		t.Fatalf("normal record must not be reported failed after it is durably stored: %v", err)
	}
	if len(repo.rows) != 1 || repo.rows[0].ID != "durable-action" || at.pendingEnd == nil {
		t.Fatalf("action should be durable and rotation retained for retry: rows=%+v pending=%v", repo.rows, at.pendingEnd)
	}
	if err := at.Record(&AuditRecord{EventID: "next-action", ActionType: "action"}); err != nil {
		t.Fatal(err)
	}
	if got := []string{repo.rows[1].ID, repo.rows[2].ID, repo.rows[3].ID}; fmt.Sprint(got) != "[epoch_end_0 epoch_start_1 next-action]" {
		t.Fatalf("next action must follow pending markers: %v", got)
	}
}

func TestRecoverRestoresEpochIDBeyondTailWindowAndScansLegacyMarker(t *testing.T) {
	t.Run("new records carry epoch ID", func(t *testing.T) {
		repo := newControlledAuditRepository()
		at := NewAuditTrail(repo, "")
		if err := at.Record(&AuditRecord{EventID: "first", ActionType: "action"}); err != nil {
			t.Fatal(err)
		}
		if err := at.RotateIfNeeded(epochSizeLimitMB); err != nil {
			t.Fatal(err)
		}
		for i := range 105 {
			if err := at.Record(&AuditRecord{EventID: fmt.Sprintf("e-%d", i), ActionType: "action"}); err != nil {
				t.Fatal(err)
			}
		}
		recovered := NewAuditTrail(repo, "")
		if err := recovered.RecoverOnStartup(); err != nil {
			t.Fatal(err)
		}
		if recovered.epochID != 1 {
			t.Fatalf("epoch ID must restore from records when marker is outside the tail: got %d", recovered.epochID)
		}
		if err := recovered.RotateIfNeeded(epochSizeLimitMB); err != nil {
			t.Fatal(err)
		}
		if repo.rows[len(repo.rows)-2].ID != "epoch_end_1" || repo.rows[len(repo.rows)-1].ID != "epoch_start_2" {
			t.Fatalf("next rotation must use unique restored IDs: %s %s", repo.rows[len(repo.rows)-2].ID, repo.rows[len(repo.rows)-1].ID)
		}
	})

	t.Run("legacy marker scan uses physical cursor", func(t *testing.T) {
		repo := newControlledAuditRepository()
		prev := ""
		start := validAuditRecord("epoch_start_3", "epoch_start", prev, nil)
		start.ActionDetail = mustJSON(map[string]any{"epoch_id": 3, "prev_epoch_final_hash": prev})
		setRecordHash(start)
		repo.appendRecord(t, start)
		prev = start.RecordHash
		for i := range 105 {
			rec := validAuditRecord(fmt.Sprintf("legacy-%d", i), "action", prev, nil)
			repo.appendRecord(t, rec)
			prev = rec.RecordHash
		}
		recovered := NewAuditTrail(repo, "")
		if err := recovered.RecoverOnStartup(); err != nil {
			t.Fatal(err)
		}
		if recovered.epochID != 3 {
			t.Fatalf("legacy epoch marker should be found beyond tail, got %d", recovered.epochID)
		}
		if err := recovered.RotateIfNeeded(epochSizeLimitMB); err != nil {
			t.Fatal(err)
		}
		if repo.rows[len(repo.rows)-2].ID != "epoch_end_3" || repo.rows[len(repo.rows)-1].ID != "epoch_start_4" {
			t.Fatalf("legacy recovery must avoid reused epoch IDs: %s %s", repo.rows[len(repo.rows)-2].ID, repo.rows[len(repo.rows)-1].ID)
		}
	})
}

func TestLegacyAuditRecordSerializationOmitsEpochID(t *testing.T) {
	record := &AuditRecord{EventID: "legacy", Timestamp: 1, ActionType: "legacy"}
	want := "{\"EventID\":\"legacy\",\"Timestamp\":1,\"AgentID\":\"\",\"SessionID\":\"\",\"ActionType\":\"legacy\",\"ActionDetail\":null,\"TrustLevel\":0,\"Authorization\":\"\",\"CapTokenID\":\"\",\"Outcome\":\"\",\"DenyReason\":\"\",\"DataSubjects\":null,\"PIIDetected\":false,\"PrevHash\":\"\",\"RecordHash\":\"\"}"
	if got := string(serializeRecord(record)); got != want {
		t.Fatalf("legacy serialization must remain byte-for-byte stable:\n got %s\nwant %s", got, want)
	}
	sum := sha256.Sum256([]byte(want))
	record.RecordHash = hex.EncodeToString(sum[:])
	if !recordHashMatches(record) {
		t.Fatal("legacy record hash must remain verifiable")
	}
}

var _ protocol.AuditRepository = (*controlledAuditRepository)(nil)
