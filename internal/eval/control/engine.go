// Package access 实现评估侧访问控制引擎（M12 §5 L1）。
//
// 职责：
//   - DataSplitter：EvalCase 来源 → 分区路由（incident/shadow → holdout，synthetic → training，manual → holdout）
//   - CheckAccess：角色 → 分区访问白名单（m9_optimizer 不可访问 holdout，ci_gate 只可访问 holdout）
//   - Engine.VerifyRequest：Ed25519 签名校验 + 时间戳防重放
//
// 依据：docs/arch/M12-Eval-Harness.md §5 三层分区 + §5.1 EvalAPI 签名约束。
package control

import (
	"crypto/ed25519"
	"fmt"
	"maps"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ── 常量 ─────────────────────────────────────────────────────────────────────

// EvalCase.Source 规范值（M12 §1）。
const (
	SourceManual    = "manual"
	SourceSynthetic = "synthetic"
	SourceIncident  = "incident"
	SourceShadow    = "shadow"
)

// 分区标识（M12 §5 DataSplitter）。
const (
	PartitionTraining   = "training"
	PartitionValidation = "validation"
	PartitionHoldout    = "holdout"

	// PartitionMetaHoldout 是比 Holdout 隔离级别更高的专属分区（V8-S2 Meta-Eval
	// Sentinel 使用），密钥与 Holdout 分离——审计"EvalHarness 目标函数是否漂移"
	// 这件事本身，不能用被审计对象也能触达的数据。不属于 DataSplitter 的四来源
	// 自动路由范围：只能由持有 RoleMetaAuditor 私钥的人工/CI 流程显式写入，
	// 禁止 SourceSynthetic/SourceIncident/SourceShadow 自动流入（00-Global-
	// Dictionary.md §V8-Principle："禁止用自动化机制替换外部锚点"）。
	PartitionMetaHoldout = "meta_holdout"
)

// AgentRole 标识调用方身份（M12 §5 L1）。
const (
	RoleM9Optimizer = "m9_optimizer" // M9 自演化引擎
	RoleCIGate      = "ci_gate"      // CI/Canary 进程外运行器

	// RoleMetaAuditor 是 meta_holdout 分区的唯一合法调用方身份（V8-S2）。
	// 私钥仅由仓库维护者持有，不得出现在运行中 server 进程的环境变量/配置里；
	// M9(m9_optimizer)/ci_gate 均不在 meta_holdout 的访问白名单内。
	RoleMetaAuditor = "meta_auditor"
)

// sigReplayWindowSec 是 Ed25519 请求签名允许的最大时钟偏差（秒）。
// 防止重放攻击，同时容忍合理的时钟漂移。
const sigReplayWindowSec = 300

// ── DataSplitter ─────────────────────────────────────────────────────────────

// DataSplitter 将 EvalCase 来源映射到目标分区（M12 §5）。
//
// 路由规则（DataSplitter 规范）：
//
//	SourceSynthetic → Training（M9 日常优化数据源）
//	SourceManual    → Holdout（人工标注黄金集）
//	SourceIncident  → Holdout（生产事故转换，仅 CI 门控）
//	SourceShadow    → Holdout（基线对比快照）
//	未知来源        → Holdout（fail-closed）
//
// allowTraining 仅对 SourceManual 生效（对应 --allow-training 标志），
// 其余来源忽略此标志。
type DataSplitter struct{}

// Partition 返回 source 对应的目标分区名称。
func (DataSplitter) Partition(source string, allowTraining bool) string {
	switch source {
	case SourceSynthetic:
		return PartitionTraining
	case SourceManual:
		if allowTraining {
			return PartitionTraining
		}
		return PartitionHoldout
	default:
		// incident、shadow 及未知来源 → holdout（fail-closed）
		return PartitionHoldout
	}
}

// ── 角色访问白名单 ─────────────────────────────────────────────────────────────

// CheckAccess 仅做角色→分区白名单检查，不涉及签名验证。
// 适用于内部子系统在已信任调用链中的快速鉴权。
func (e *Engine) CheckAccess(agentRole, partition string) error {
	allowed, ok := e.allowedPartitions[agentRole]
	if !ok {
		return apperr.New(apperr.CodeUnauthorized,
			fmt.Sprintf("eval_policy: unknown agent role %q", agentRole))
	}
	if _, ok := allowed[partition]; !ok {
		return apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("eval_policy: role %q is not allowed to access partition %q", agentRole, partition))
	}
	return nil
}

// ── Engine ───────────────────────────────────────────────────────────────────

// Engine 执行评估层完整访问策略：角色白名单 + Ed25519 签名校验（M12 §5 L1）。
//
// 签名消息格式（签名端与验证端必须一致）：
//
//	{agentRole}:{partition}:{unix_timestamp}
//
// 公钥通过 NewEngine 注入；未注册角色的请求一律拒绝（fail-closed）。
type Engine struct {
	pubKeys           map[string]ed25519.PublicKey
	allowedPartitions map[string]map[string]struct{}
	Splitter          DataSplitter
}

// NewEngine 创建引擎，pubKeys 为 agentRole → Ed25519 公钥的映射。
// 生产环境应从 OS Keychain 或配置中心加载持久化密钥对；
// 测试环境可用 crypto/ed25519.GenerateKey 临时生成。
func NewEngine(pubKeys map[string]ed25519.PublicKey) *Engine {
	keys := make(map[string]ed25519.PublicKey, len(pubKeys))
	maps.Copy(keys, pubKeys)
	return &Engine{
		pubKeys: keys,
		allowedPartitions: map[string]map[string]struct{}{
			RoleM9Optimizer: {
				PartitionTraining:   {},
				PartitionValidation: {},
			},
			RoleCIGate: {
				PartitionHoldout: {},
			},
			RoleMetaAuditor: {
				PartitionMetaHoldout: {},
			},
		},
	}
}

// VerifyRequest 执行完整请求验证：
//  1. 角色→分区白名单检查（fail-closed）
//  2. 时间戳防重放（|now - timestamp| ≤ sigReplayWindowSec）
//  3. Ed25519 签名校验（用注册公钥验证 "{agentRole}:{partition}:{timestamp}" 的签名）
//
// 若角色无注册公钥则拒绝请求，防止无密钥调用者绕过签名门。
func (e *Engine) VerifyRequest(agentRole, partition string, signature []byte, timestamp int64) error {
	// 步骤 1：角色访问白名单（提前拦截，避免无效签名运算）
	if err := e.CheckAccess(agentRole, partition); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Engine.VerifyRequest", err)
	}

	// 步骤 2：防重放时间窗口
	drift := time.Now().Unix() - timestamp
	if drift < 0 {
		drift = -drift
	}
	if drift > sigReplayWindowSec {
		return apperr.New(apperr.CodeUnauthorized,
			fmt.Sprintf("eval_policy: request timestamp out of window (drift=%ds, max=%ds)", drift, sigReplayWindowSec))
	}

	// 步骤 3：公钥注册检查 + 签名验证
	pubKey, ok := e.pubKeys[agentRole]
	if !ok {
		return apperr.New(apperr.CodeUnauthorized,
			fmt.Sprintf("eval_policy: no public key registered for role %q (fail-closed)", agentRole))
	}
	msg := fmt.Appendf(nil, "%s:%s:%d", agentRole, partition, timestamp)
	if !ed25519.Verify(pubKey, msg, signature) {
		return apperr.New(apperr.CodeUnauthorized,
			fmt.Sprintf("eval_policy: signature verification failed for role %q", agentRole))
	}
	return nil
}

// VerifyRequestDev 是仅供开发/测试环境使用的宽松变体：
// 当角色无注册公钥时，跳过签名验证并仅执行角色访问白名单检查。
//
// 禁止在生产部署中使用（角色不受签名约束等同于无认证）。
// 对应 store.go 中 "MVP: 忽略签名校验" 的临时占位行为的迁移路径。
func (e *Engine) VerifyRequestDev(agentRole, partition string, signature []byte, timestamp int64) error {
	if err := e.CheckAccess(agentRole, partition); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Engine.VerifyRequestDev", err)
	}
	if _, ok := e.pubKeys[agentRole]; !ok {
		// dev/test 模式：无注册密钥时仅做访问白名单检查
		return nil
	}
	return e.VerifyRequest(agentRole, partition, signature, timestamp)
}
