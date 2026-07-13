package harness

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteEvalStore 实现了 protocol.EvalAPI，用于管理 EvalCase。
// 数据持久化基于 protocol.Store (SQLite 驱动)。
// 架构文档: docs/arch/M12-Eval-Harness.md §5
type SQLiteEvalStore struct {
	store  protocol.Store
	engine *control.Engine
}

var _ protocol.EvalAPI = (*SQLiteEvalStore)(nil)

func NewSQLiteEvalStore(store protocol.Store, engine *control.Engine) *SQLiteEvalStore {
	return &SQLiteEvalStore{store: store, engine: engine}
}

// GetTrainingCases 获取用于训练和优化的评测用例 (Training Set)。
func (s *SQLiteEvalStore) GetTrainingCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) {
	if err := verifyEvalSignature(agentRole, control.PartitionTraining, signature); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.GetTrainingCases", err)
	}
	if err := s.engine.CheckAccess(agentRole, control.PartitionTraining); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.GetTrainingCases", err)
	}
	return s.scanCasesByPrefix(ctx, "eval:case:training:"+agentRole+":")
}

// GetValidationCases 获取用于泛化验证的评测用例 (Holdout Set)。
func (s *SQLiteEvalStore) GetValidationCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) {
	if err := verifyEvalSignature(agentRole, control.PartitionValidation, signature); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.GetValidationCases", err)
	}
	if err := s.engine.CheckAccess(agentRole, control.PartitionValidation); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.GetValidationCases", err)
	}
	return s.scanCasesByPrefix(ctx, "eval:case:validation:"+agentRole+":")
}

// GetMetaHoldoutCases 获取 V8-S2 Meta-Eval Sentinel 专属分区的评测用例。
//
// 注意：本方法故意不在 protocol.EvalAPI 接口中暴露——该接口是"暴露给自进化引擎
// (M9) 的只读数据接口"，而 meta_holdout 存在的唯一意义就是不能被 M9 触达
// （00-Global-Dictionary.md §V8-Principle）。调用方必须以 control.RoleMetaAuditor
// 身份签名，且该角色私钥不应出现在运行中 server 进程里——合法调用方只应是脱离
// 主进程的人工/CI 审计流程，通过本方法的具体类型直接引用（而非走 EvalAPI）。
func (s *SQLiteEvalStore) GetMetaHoldoutCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) {
	if err := verifyEvalSignature(agentRole, control.PartitionMetaHoldout, signature); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.GetMetaHoldoutCases", err)
	}
	if err := s.engine.CheckAccess(agentRole, control.PartitionMetaHoldout); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.GetMetaHoldoutCases", err)
	}
	return s.scanCasesByPrefix(ctx, "eval:case:meta_holdout:"+agentRole+":")
}

// PutMetaHoldoutCase 写入一条 meta_holdout 分区用例，要求 control.RoleMetaAuditor
// 签名。这是 meta_holdout 唯一合法的写入路径——不同于 PutCase（供内部可信管线
// 直接调用，本身不做鉴权），本方法面向可能被 HTTP 层暴露的场景，必须携带有效
// 签名，否则任何持有 *SQLiteEvalStore 引用的调用方都能绕过隔离直接写入。
func (s *SQLiteEvalStore) PutMetaHoldoutCase(ctx context.Context, c EvalCase, signature []byte) error {
	if err := verifyEvalSignature(control.RoleMetaAuditor, control.PartitionMetaHoldout, signature); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.PutMetaHoldoutCase", err)
	}
	if err := s.engine.CheckAccess(control.RoleMetaAuditor, control.PartitionMetaHoldout); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.PutMetaHoldoutCase", err)
	}
	return s.PutCase(ctx, control.PartitionMetaHoldout, control.RoleMetaAuditor, c)
}

// PutCase 保存一个新的 EvalCase 到指定分区 (training 或 validation)。
func (s *SQLiteEvalStore) PutCase(ctx context.Context, partition, agentRole string, c EvalCase) error {
	validPartitions := map[string]bool{
		control.PartitionTraining:    true,
		control.PartitionValidation:  true,
		"pending_review":             true,
		control.PartitionMetaHoldout: true, // V8-S2: Meta-Eval 专属分区，密钥与 Holdout 分离
	}
	if !validPartitions[partition] {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("eval_store: invalid partition %s", partition))
	}
	key := fmt.Sprintf("eval:case:%s:%s:%s", partition, agentRole, c.ID)
	data, err := json.Marshal(c)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.PutCase", err)
	}
	return s.store.Put(ctx, []byte(key), data)
}

func (s *SQLiteEvalStore) scanCasesByPrefix(ctx context.Context, prefix string) ([]any, error) {
	iter, err := s.store.Scan(ctx, []byte(prefix))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.scanCasesByPrefix", err)
	}
	defer iter.Close()

	var cases []any
	for iter.Next() {
		var c EvalCase
		if err := json.Unmarshal(iter.Value(), &c); err == nil {
			cases = append(cases, c)
		}
	}
	return cases, nil
}

// PromotePendingCase 将指定用例从源分区迁移到目标分区，并记录审核人和调整后的级别。
// agentRole 必须与 PutCase 写入时传入的值一致，key 结构为 eval:case:{partition}:{agentRole}:{caseID}。
func (s *SQLiteEvalStore) PromotePendingCase(ctx context.Context, caseID, fromPartition, toPartition, reviewer, agentRole string, adjustedSeverity Severity) error {
	oldKey := fmt.Sprintf("eval:case:%s:%s:%s", fromPartition, agentRole, caseID)

	val, err := s.store.Get(ctx, []byte(oldKey))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to get pending case", err)
	}
	if val == nil {
		return apperr.New(apperr.CodeNotFound, "eval_store: pending case not found")
	}

	var c EvalCase
	if err := json.Unmarshal(val, &c); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to unmarshal pending case", err)
	}

	// 记录审核信息（可以存入 metadata 或类似字段，由于 EvalCase 没有 reviewer 字段，仅做 severity 修改）
	c.Severity = adjustedSeverity

	// 保存到新分区
	if err := s.PutCase(ctx, toPartition, agentRole, c); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to put case to new partition", err)
	}

	// 删除旧分区数据
	return s.store.Delete(ctx, []byte(oldKey))
}

// GetPassRateAvgSince 查询指定时间后的平均通过率（按 suite="validation" 过滤）。
func (s *SQLiteEvalStore) GetPassRateAvgSince(ctx context.Context, since time.Time) (float64, error) {
	iter, err := s.store.Scan(ctx, []byte("eval:run:validation:"))
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "failed to scan store", err)
	}
	defer iter.Close()

	var totalPassRate float64
	var count int

	for iter.Next() {
		var payload types.EvalCompletedPayload
		if err := json.Unmarshal(iter.Value(), &payload); err == nil {
			if time.Unix(payload.CreatedAt, 0).After(since) {
				totalPassRate += payload.PassRate
				count++
			}
		}
	}
	if count == 0 {
		return 0, nil
	}
	return totalPassRate / float64(count), nil
}

// MetaAuditRecord 是 V8-S2 Meta-Eval Sentinel 单次审计结论的持久化记录。
// 只保留"最新一次"结论（key 固定为 "eval:meta_audit:latest"，非按版本/时间累积）——
// MetaEvalSentinel 审计的是 EvalHarness 目标函数本身是否漂移，这是一个全局属性，
// 不像 Training/Validation 那样需要按 candidate 版本区分。
type MetaAuditRecord struct {
	Passed               bool
	MedianFalsifiability float64
	TotalCases           int
	Reasons              []string
	ComputedAt           int64 // unix 秒
}

const metaAuditLatestKey = "eval:meta_audit:latest"

// RecordMetaAuditResult 持久化最新一次 Meta-Eval 审计结论，供 AdvanceGate 只读消费。
// 不做独立签名校验——调用方（MetaEvalSentinel.RunAndRecord）在写入前已通过
// GetMetaHoldoutCases 的签名校验成功读取过 meta_holdout，身份已在同一次调用链中
// 验证过，无需重复校验。
func (s *SQLiteEvalStore) RecordMetaAuditResult(ctx context.Context, rec MetaAuditRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.RecordMetaAuditResult", err)
	}
	if err := s.store.Put(ctx, []byte(metaAuditLatestKey), data); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.RecordMetaAuditResult: store put failed", err)
	}
	return nil
}

// LatestMetaAudit 读取最新一次 Meta-Eval 审计结论。ok=false 表示从未审计过
// （尚未配置/运行过 meta_holdout 审计流程）。本方法故意不做签名校验——返回的只是
// pass/fail 摘要，不暴露 meta_holdout 原始用例数据，供 AdvanceGate/运维状态查询
// 等进程内场景自由调用；结构上满足 internal/prompt/optimizer.MetaAuditReader
// 消费方接口（HE-3：接口由调用方定义），无需适配器。
func (s *SQLiteEvalStore) LatestMetaAudit(ctx context.Context) (passed bool, computedAt time.Time, ok bool, err error) {
	val, getErr := s.store.Get(ctx, []byte(metaAuditLatestKey))
	if getErr != nil {
		return false, time.Time{}, false, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.LatestMetaAudit", getErr)
	}
	if val == nil {
		return false, time.Time{}, false, nil
	}
	var rec MetaAuditRecord
	if jsonErr := json.Unmarshal(val, &rec); jsonErr != nil {
		return false, time.Time{}, false, apperr.Wrap(apperr.CodeInternal, "SQLiteEvalStore.LatestMetaAudit: unmarshal", jsonErr)
	}
	return rec.Passed, time.Unix(rec.ComputedAt, 0), true, nil
}

// verifyEvalSignature 校验 agentRole 对 payload 的 Ed25519 签名。
// 若系统未配置对应 agentRole 的公钥（开发环境），仅记录 WARN 并放行；
// 若已配置公钥，签名无效则返回 ErrInvalidSignature。
// payload 格式：agentRole + ":" + partition + ":" + UTC 分钟级时间戳（防重放 ±2min）
func verifyEvalSignature(agentRole, partition string, signature []byte) error {
	pubKey := evalPublicKey(agentRole) // 从环境变量或配置文件读取
	if pubKey == nil {
		// 未配置公钥：开发/Tier-0 模式，仅告警
		slog.Warn("eval signature not verified: no public key configured",
			"agent_role", agentRole, "partition", partition)
		return nil
	}
	if len(signature) == 0 {
		return apperr.New(apperr.CodeForbidden, "eval_store: signature required")
	}
	// payload = agentRole:partition:YYYYMMDDHHmm（UTC 分钟，±2min 容差）
	now := time.Now().UTC()
	for _, t := range []time.Time{now, now.Add(-time.Minute), now.Add(time.Minute),
		now.Add(-2 * time.Minute), now.Add(2 * time.Minute)} {
		payload := []byte(agentRole + ":" + partition + ":" + t.Format("200601021504"))
		if ed25519.Verify(pubKey, payload, signature) {
			return nil
		}
	}
	return apperr.New(apperr.CodeForbidden, "eval_store: invalid signature")
}

// evalPublicKey 从环境变量 POLARIS_EVAL_PUBKEY_<ROLE> 读取 base64 编码的 Ed25519 公钥。
// 返回 nil 表示未配置（放行模式）。
func evalPublicKey(agentRole string) ed25519.PublicKey {
	envKey := "POLARIS_EVAL_PUBKEY_" + strings.ToUpper(strings.ReplaceAll(agentRole, "-", "_"))
	b64 := os.Getenv(envKey)
	if b64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}
