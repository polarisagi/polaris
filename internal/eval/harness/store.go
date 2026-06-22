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
		return nil, fmt.Errorf("SQLiteEvalStore.GetTrainingCases: %w", err)
	}
	if err := s.engine.CheckAccess(agentRole, control.PartitionTraining); err != nil {
		return nil, fmt.Errorf("SQLiteEvalStore.GetTrainingCases: %w", err)
	}
	return s.scanCasesByPrefix(ctx, "eval:case:training:"+agentRole+":")
}

// GetValidationCases 获取用于泛化验证的评测用例 (Holdout Set)。
func (s *SQLiteEvalStore) GetValidationCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) {
	if err := verifyEvalSignature(agentRole, control.PartitionValidation, signature); err != nil {
		return nil, fmt.Errorf("SQLiteEvalStore.GetValidationCases: %w", err)
	}
	if err := s.engine.CheckAccess(agentRole, control.PartitionValidation); err != nil {
		return nil, fmt.Errorf("SQLiteEvalStore.GetValidationCases: %w", err)
	}
	return s.scanCasesByPrefix(ctx, "eval:case:validation:"+agentRole+":")
}

// PutCase 保存一个新的 EvalCase 到指定分区 (training 或 validation)。
func (s *SQLiteEvalStore) PutCase(ctx context.Context, partition, agentRole string, c EvalCase) error {
	validPartitions := map[string]bool{
		"training":       true,
		"validation":     true,
		"pending_review": true,
		"meta_holdout":   true, // V8-S2: Meta-Eval 专属分区，密钥与 Holdout 分离
	}
	if !validPartitions[partition] {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("eval_store: invalid partition %s", partition))
	}
	key := fmt.Sprintf("eval:case:%s:%s:%s", partition, agentRole, c.ID)
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("SQLiteEvalStore.PutCase: %w", err)
	}
	return s.store.Put(ctx, []byte(key), data)
}

func (s *SQLiteEvalStore) scanCasesByPrefix(ctx context.Context, prefix string) ([]any, error) {
	iter, err := s.store.Scan(ctx, []byte(prefix))
	if err != nil {
		return nil, fmt.Errorf("SQLiteEvalStore.scanCasesByPrefix: %w", err)
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
