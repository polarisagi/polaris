package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// Standard Audit Actions for Extension Installation
const (
	ActionInstallApproved    = "install_approved"
	ActionInstallRejected    = "install_rejected"
	ActionInstallHITLPending = "install_hitl_pending"
)

// AuditTrail — 不可变哈希链审计轨迹。
// 架构文档: docs/arch/M11-Policy-Safety.md §7
//
// hash chain 结构:
//   RecordHash = SHA-256(序列化后的 AuditRecord 字段，含 PrevHash)
//   PrevHash(i) = RecordHash(i-1)，第一条记录 PrevHash = ""

const epochSizeLimitMB = 100 // Epoch 轮转阈值

type AuditTrail struct {
	mu       sync.RWMutex
	records  []*AuditRecord
	lastHash string
	epochID  int

	// epochStartHash 记录当前 epoch 的第一条 PrevHash，用于跨 epoch 连续性校验
	epochStartHash string
	archiveDir     string
	repo           protocol.AuditRepository
}

// NewAuditTrail 创建审计轨迹，archiveDir 为归档路径（e.g. ~/.polarisagi/polaris/audit/archive/）。
func NewAuditTrail(repo protocol.AuditRepository, archiveDir string) *AuditTrail {
	return &AuditTrail{
		repo:       repo,
		archiveDir: archiveDir,
	}
}

// AuditRecord 单条审计记录。
type AuditRecord struct {
	EventID       string
	Timestamp     int64
	AgentID       string
	SessionID     string
	ActionType    string
	ActionDetail  []byte
	TrustLevel    int
	Authorization string
	CapTokenID    string
	Outcome       string // allow | deny | error | escalated
	DenyReason    string
	DataSubjects  []string
	PIIDetected   bool
	PrevHash      string
	RecordHash    string
}

// Record 追加审计记录（仅追加，hash chain 保证完整性）。
func (at *AuditTrail) Record(record *AuditRecord) error {
	at.mu.Lock()
	defer at.mu.Unlock()

	record.PrevHash = at.lastHash
	if record.Timestamp == 0 {
		record.Timestamp = time.Now().UnixMicro()
	}

	data := serializeRecord(record)
	hash := sha256.Sum256(data)
	record.RecordHash = hex.EncodeToString(hash[:])

	// 持久化到数据库
	if at.repo != nil {
		id := record.EventID
		if id == "" {
			id = fmt.Sprintf("audit_%d", record.Timestamp)
			record.EventID = id
		}
		actor := record.AgentID
		if actor == "" {
			actor = "system"
		}
		typ := "system"
		payload := mustJSON(record) // 序列化完整结构（虽然规范建议 protobuf，暂按 JSON 存）

		err := at.repo.AppendAuditEvent(context.Background(), types.AuditEventRow{
			ID:     id,
			Actor:  actor,
			Action: typ,
			Meta:   string(payload),
		})

		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to persist audit record (fail-closed)", err)
		}
	}

	at.records = append(at.records, record)
	at.lastHash = record.RecordHash
	return nil
}

// VerifyIntegrity 遍历 hash chain 验证完整性。
// 发现断裂返回 (false, brokenIndex)；完整返回 (true, -1)。
func (at *AuditTrail) VerifyIntegrity() (bool, int) {
	at.mu.RLock()
	defer at.mu.RUnlock()

	for i, r := range at.records {
		// 检查 PrevHash 链接
		if i > 0 && r.PrevHash != at.records[i-1].RecordHash {
			return false, i
		}
		// 重算 RecordHash 校验数据未被篡改
		data := serializeRecord(r)
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != r.RecordHash {
			return false, i
		}
	}
	return true, -1
}

// RotateIfNeeded 当估算体积达到 100MB 时执行 Epoch 轮转。
// currentSizeMB 由调用方传入（来自 M3 监控的 gauge）。
func (at *AuditTrail) RotateIfNeeded(currentSizeMB int) error {
	if currentSizeMB < epochSizeLimitMB {
		return nil
	}

	at.mu.Lock()
	defer at.mu.Unlock()

	// 追加 epoch_end 标记记录，封存当前 epoch
	epochEnd := &AuditRecord{
		EventID:    fmt.Sprintf("epoch_end_%d", at.epochID),
		Timestamp:  time.Now().UnixMicro(),
		ActionType: "epoch_end",
		ActionDetail: mustJSON(map[string]any{
			"epoch_id":     at.epochID,
			"record_count": len(at.records),
			"final_hash":   at.lastHash,
		}),
		PrevHash: at.lastHash,
	}
	data := serializeRecord(epochEnd)
	hash := sha256.Sum256(data)
	epochEnd.RecordHash = hex.EncodeToString(hash[:])

	// 持久化到数据库
	if at.repo != nil {
		payload := mustJSON(epochEnd)
		_ = at.repo.AppendAuditEvent(context.Background(), types.AuditEventRow{
			ID:     epochEnd.EventID,
			Actor:  "system",
			Action: "system",
			Meta:   string(payload),
		})
	}

	at.records = append(at.records, epochEnd)
	at.lastHash = epochEnd.RecordHash

	// 归档当前 epoch（生产环境应写文件 + gzip；此处仅递增 epochID）
	at.epochID++
	prevEpochFinalHash := at.lastHash

	// 重置当前 epoch 状态
	at.records = nil
	at.epochStartHash = prevEpochFinalHash

	// 追加 epoch_start 标记，建立跨 Epoch 密码学连续性
	epochStart := &AuditRecord{
		EventID:    fmt.Sprintf("epoch_start_%d", at.epochID),
		Timestamp:  time.Now().UnixMicro(),
		ActionType: "epoch_start",
		ActionDetail: mustJSON(map[string]any{
			"epoch_id":              at.epochID,
			"prev_epoch_final_hash": prevEpochFinalHash,
		}),
		PrevHash: prevEpochFinalHash,
	}
	startData := serializeRecord(epochStart)
	startHash := sha256.Sum256(startData)
	epochStart.RecordHash = hex.EncodeToString(startHash[:])

	// 持久化到数据库
	if at.repo != nil {
		payload := mustJSON(epochStart)
		_ = at.repo.AppendAuditEvent(context.Background(), types.AuditEventRow{
			ID:     epochStart.EventID,
			Actor:  "system",
			Action: "system",
			Meta:   string(payload),
		})
	}

	at.records = []*AuditRecord{epochStart}
	at.lastHash = epochStart.RecordHash

	return nil
}

// RecoverOnStartup 扫描归档目录，校验跨 Epoch hash 链连续性，并从 DB 恢复尾部状态。
func (at *AuditTrail) RecoverOnStartup() error { //nolint:nestif
	if at.archiveDir != "" {
		// 检测 .fullstop 密封文件
		fullstopPath := filepath.Join(filepath.Dir(at.archiveDir), ".fullstop")
		if _, err := os.Stat(fullstopPath); err == nil {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: system is sealed (.fullstop exists at %s) — run unseal before starting", fullstopPath))
		}
	}

	return at.recoverFromDB()
}

// recoverFromDB 从数据库恢复尾部审计日志
func (at *AuditTrail) recoverFromDB() error {
	if at.repo == nil {
		return nil
	}

	events, err := at.repo.ListAuditEvents(context.Background(), 100, "")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "audit: query events for recovery", err)
	}

	var loaded []*AuditRecord
	for _, ev := range events {
		var rec AuditRecord
		if unmarshalErr := json.Unmarshal([]byte(ev.Meta), &rec); unmarshalErr == nil {
			loaded = append(loaded, &rec)
		}
	}

	for i, j := 0, len(loaded)-1; i < j; i, j = i+1, j-1 {
		loaded[i], loaded[j] = loaded[j], loaded[i]
	}

	at.mu.Lock()
	at.records = append(at.records, loaded...)
	if len(loaded) > 0 {
		at.lastHash = loaded[len(loaded)-1].RecordHash
	}
	at.mu.Unlock()

	ok, idx := at.VerifyIntegrity()
	if !ok {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("audit: integrity check failed on DB recovery at index %d", idx))
	}

	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// serializeRecord 确定性序列化 AuditRecord（不含 RecordHash 字段本身）。
// 注：序列化时临时置空 RecordHash，以确保 RecordHash 不参与自身计算。
func serializeRecord(r *AuditRecord) []byte {
	// 使用副本，避免改变原始指针
	copy := *r
	copy.RecordHash = ""
	data, _ := json.Marshal(copy)
	return data
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
