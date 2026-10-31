package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
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

	// pendingEnd/pendingStart 保存轮转的稳定 marker；持久化结果不确定时可按原 ID 和原 Meta 重试。
	pendingEnd   *AuditRecord
	pendingStart *AuditRecord
	// pendingRecord 阻止普通追加在存储结果未确认时推进内存哈希链。
	pendingRecord       *AuditRecord
	pendingRecordCaller *AuditRecord

	// epochStartHash 记录当前 epoch 的第一条 PrevHash，用于跨 epoch 连续性校验
	epochStartHash string
	archiveDir     string
	repo           protocol.AuditRepository

	// epochBytes 当前 epoch 已追加记录的序列化字节数（GR-2.1-005）：轮转由 Record 自驱动，
	// 不再依赖从未有人提供的外部体积 gauge——否则 records 常驻内存无上界。
	epochBytes int64
}

// NewAuditTrail 创建审计轨迹，archiveDir 为归档路径（e.g. ~/.polaris/audit/archive/）。
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
	EpochID       *int `json:",omitempty"`
}

// Record 追加审计记录（仅追加，hash chain 保证完整性）。
func (at *AuditTrail) Record(record *AuditRecord) error {
	if record == nil {
		return apperr.New(apperr.CodeInvalidInput, "audit: record is nil")
	}
	at.mu.Lock()
	defer at.mu.Unlock()
	pendingRetry := at.isPendingRecordRetryLocked(record)
	if err := at.finishPendingRotationLocked(); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "audit: pending epoch rotation prevents new records", err)
	}
	if pendingRetry {
		return nil
	}

	stable := cloneAuditRecord(record)
	stable.PrevHash = at.lastHash
	stable.EpochID = epochIDPointer(at.epochID)
	if stable.Timestamp == 0 {
		stable.Timestamp = time.Now().UnixMicro()
	}
	// EventID 必须在算 hash **之前**补齐。
	//
	// 2026-08-11 修复：此前这段补齐逻辑写在下方持久化分支里（`if at.repo != nil`
	// 内），即 **hash 先算、EventID 后填**，而落库存的是填完 EventID 的版本。
	// 重启恢复时按存量记录重算 hash，自然对不上——于是
	// `VerifyIntegrity` 报 "integrity check failed at index 0"，
	// `RecoverOnStartup` 返回错误，`bootSubstrate` 直接中止启动。
	//
	// 后果不是边角情况：EventID 走自动生成是常规路径（调用方通常不自己指定），
	// 且只在 at.repo != nil（生产）时触发——**只要有审计记录落库，下一次重启
	// 必然失败**。用户实测：开发库里仅 1 条审计记录，重启即 brick。
	//
	// 凡是参与持久化的字段都必须在 hash 之前定型；hash 之后再改结构体，等于
	// 存下一个自己验不过的记录。
	if stable.EventID == "" {
		stable.EventID = fmt.Sprintf("audit_%d", stable.Timestamp)
	}

	data := serializeRecord(stable)
	hash := sha256.Sum256(data)
	stable.RecordHash = hex.EncodeToString(hash[:])
	copyAuditRecord(record, stable)

	at.pendingRecord = stable
	at.pendingRecordCaller = record
	if err := at.finishPendingRecordLocked(); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "failed to persist audit record (fail-closed)", err)
	}

	if at.epochBytes >= epochSizeLimitMB<<20 {
		if err := at.rotateLocked(); err != nil {
			// 当前业务审计记录已确认落库，不能把轮转失败返回成该动作失败诱发重做；
			// 保留 pending marker，并在后续 Record 前强制补齐轮转。
			slog.Error("audit_trail: epoch rotation pending after durable record",
				"event", stable.EventID, "error_type", fmt.Sprintf("%T", err), "code", string(apperr.CodeOf(err)))
		}
	}
	return nil
}

func cloneAuditRecord(record *AuditRecord) *AuditRecord {
	clone := *record
	clone.ActionDetail = append([]byte(nil), record.ActionDetail...)
	clone.DataSubjects = append([]string(nil), record.DataSubjects...)
	if record.EpochID != nil {
		epochID := *record.EpochID
		clone.EpochID = &epochID
	}
	return &clone
}

func copyAuditRecord(dst, src *AuditRecord) {
	clone := cloneAuditRecord(src)
	*dst = *clone
}

func (at *AuditTrail) isPendingRecordRetryLocked(record *AuditRecord) bool {
	if at.pendingRecord == nil {
		return false
	}
	if record == at.pendingRecordCaller {
		return record.RecordHash == at.pendingRecord.RecordHash && string(serializeRecord(record)) == string(serializeRecord(at.pendingRecord))
	}
	return record.EventID != "" && record.EventID == at.pendingRecord.EventID &&
		record.RecordHash == at.pendingRecord.RecordHash &&
		string(serializeRecord(record)) == string(serializeRecord(at.pendingRecord))
}

// RotateIfNeeded 当估算体积达到 100MB 时执行 Epoch 轮转。
// currentSizeMB 由调用方传入（来自 M3 监控的 gauge）。
//
// 生产路径由 Record 按 epochBytes 自动触发；本方法保留给需要强制轮转的调用方（及测试）。
func (at *AuditTrail) RotateIfNeeded(currentSizeMB int) error {
	if currentSizeMB < epochSizeLimitMB {
		return nil
	}

	at.mu.Lock()
	defer at.mu.Unlock()
	return at.rotateLocked()
}

// rotateLocked 封存当前 epoch 并开启新 epoch（须持有 at.mu）。
func (at *AuditTrail) rotateLocked() error {
	if err := at.finishPendingRecordLocked(); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "audit: cannot rotate before pending record confirmation", err)
	}
	if at.pendingEnd == nil && at.pendingStart == nil {
		at.pendingEnd = newEpochEnd(at.epochID, at.lastHash, len(at.records))
	}
	return at.finishPendingRotationLocked()
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
	return at.recoverAuditTail()
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

// RecordAudit 实现 dispatch.AuditLogger 接口（internal/tool/dispatch）。
// 2026-07-14 订正：此前注释误写"实现 protocol.AuditLogger 接口"——两者是不同
// 签名的接口（protocol.AuditLogger 是 Log(ctx, action, meta)），AuditTrail 从未
// 实现过 protocol.AuditLogger；该接口现通过 cmd/polaris 的
// auditTrailLogAdapter 桥接使用，AuditTrail 本身签名不变。
func (at *AuditTrail) RecordAudit(ctx context.Context, toolName string, payload []byte) error {
	var meta map[string]any
	var agentID, sessionID string
	if err := json.Unmarshal(payload, &meta); err == nil {
		if a, ok := meta["agent_id"].(string); ok {
			agentID = a
		}
		if s, ok := meta["session_id"].(string); ok {
			sessionID = s
		}
	}

	record := &AuditRecord{
		ActionType:   "tool_execute:" + toolName,
		ActionDetail: payload,
		Timestamp:    time.Now().UnixMicro(),
		AgentID:      agentID,
		SessionID:    sessionID,
	}
	return at.Record(record)
}
