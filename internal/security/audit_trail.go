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
	if record.EventID == "" {
		record.EventID = fmt.Sprintf("audit_%d", record.Timestamp)
	}

	data := serializeRecord(record)
	hash := sha256.Sum256(data)
	record.RecordHash = hex.EncodeToString(hash[:])

	// 持久化到数据库
	if at.repo != nil {
		id := record.EventID
		actor := record.AgentID
		if actor == "" {
			actor = "system"
		}
		typ := "system"
		payload := mustJSON(record) // 序列化完整结构（虽然规范建议 protobuf，暂按 JSON 存）

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := at.repo.AppendAuditEvent(ctx, types.AuditEventRow{
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := at.repo.AppendAuditEvent(ctx, types.AuditEventRow{
			ID:     epochEnd.EventID,
			Actor:  "system",
			Action: "system",
			Meta:   string(payload),
		}); err != nil {
			slog.Error("audit_trail: append epoch marker failed", "event", epochEnd.EventID, "err", err)
		}
		cancel()
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := at.repo.AppendAuditEvent(ctx, types.AuditEventRow{
			ID:     epochStart.EventID,
			Actor:  "system",
			Action: "system",
			Meta:   string(payload),
		}); err != nil {
			slog.Error("audit_trail: append epoch marker failed", "event", epochStart.EventID, "err", err)
		}
		cancel()
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := at.repo.ListAuditEvents(ctx, 100, "")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "audit: query events for recovery", err)
	}

	var loaded []*AuditRecord
	for i, ev := range events {
		var rec AuditRecord
		if unmarshalErr := json.Unmarshal([]byte(ev.Meta), &rec); unmarshalErr == nil {
			if i == 0 && rec.PrevHash != "" {
				// 回收窗口内首条已有 PrevHash，说明恢复范围前尚有更早记录未包含，
				// 渡陶账延证可能存在盲区，记录 WARN 供操作员周知
				slog.Warn("audit_trail: recovery window truncated, hash chain verification before offset is unavailable",
					"first_offset", ev.ID, "prev_hash", rec.PrevHash)
			}
			loaded = append(loaded, &rec)
		}
	}

	for i, j := 0, len(loaded)-1; i < j; i, j = i+1, j-1 {
		loaded[i], loaded[j] = loaded[j], loaded[i]
	}

	at.mu.Lock()
	if len(loaded) > 0 {
		// 额外查询被截断边界前一条记录的 hash 作为校验锚点 (GR-2-007)
		oldestCreatedAt := events[len(events)-1].CreatedAt
		boundaryEvents, err := at.repo.ListAuditEvents(ctx, 1, oldestCreatedAt)
		if err == nil && len(boundaryEvents) > 0 {
			var anchorRec AuditRecord
			if json.Unmarshal([]byte(boundaryEvents[0].Meta), &anchorRec) == nil {
				at.epochStartHash = anchorRec.RecordHash
			}
		}
	}
	at.records = append(at.records, loaded...)
	if len(loaded) > 0 {
		at.lastHash = loaded[len(loaded)-1].RecordHash
	}
	at.mu.Unlock()

	at.mu.RLock()
	idx, why := at.verifyIntegrityLocked()
	total := len(at.records)
	at.mu.RUnlock()
	if idx >= 0 {
		// 报错必须说清「哪一条、哪项检查、看到了什么」。原实现只有
		// "integrity check failed on DB recovery at index 0"，运维拿到它既不知道
		// 是链接断了还是内容被改，也不知道下一步该看什么——而这条错误会直接
		// 中止启动，是最需要可诊断性的位置。
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"audit: 审计链完整性校验失败，拒绝启动。\n"+
				"  断裂位置: 第 %d 条（共恢复 %d 条）\n"+
				"  失败原因: %s\n"+
				"  审计事件存于 events 表 topic='audit.policy'。\n"+
				"  这是 fail-closed 设计：审计链是安全事件的唯一事后凭据，"+
				"带着一条已知断裂的链继续运行，等于让后续所有审计记录都失去证明力。",
			idx, total, why))
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
