package hitl

import (
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// HITL 自适应降级（GD-14-004 / ADR-0088 决策四）
//
// 问题：审批频率过高会导致"审批疲劳"（habituation）——用户对反复出现的同类
// 弹窗形成习惯性点击，安全防线名存实亡。2026 年大规模自主 Agent 的运营经验
// 反复印证这一点：一个每天弹 50 次的确认框，其真实安全价值接近于零。
//
// 但自适应降级本身是在**削弱安全边界**，因此本实现的设计原则是
// "默认关闭 + 多重硬地板 + 只降一档"，而不是"聪明地自动放行"：
//
//  1. 默认关闭（MinApprovals=0）。用户不显式配置就完全等价于未引入本机制。
//  2. 只对"低风险 + 无污点 + 非设备操控"的 checkpoint 生效，且**只降到通知**
//     （NotifyOnly），永远不会降到"静默放行"。
//  3. 硬地板不可穿透：TaintLevel>=TaintMedium、RiskLevel>=3、
//     CheckpointDeviceControlReview 一律不参与降级——这与
//     resolveTimeoutAction 中的既有地板保持同一组条件，不新增例外。
//  4. 信任只在"同一 (checkpoint_type, agent) 且近期人工批准率 100%"时累积；
//     出现任何一次人工拒绝立即清零，重新开始累积。
//
// 观测先行的关系：polaris.hitl.decisions_total 提供的是**是否需要**开启
// 降级的判断依据（哪些 checkpoint_type 的 human 批准率长期接近 100%），
// 本文件提供的是**开启后**的执行机制。两者互补，缺一不可。
// ============================================================================

// TrustPolicy 自适应降级策略。零值即"完全关闭"。
type TrustPolicy struct {
	// MinApprovals 连续人工批准多少次后开始降级；<=0 表示关闭本机制。
	MinApprovals int
	// Window 信任累积的有效期。超过此时长未再次出现同类审批，计数清零——
	// 信任不应无限期留存，"三个月前批过 10 次"不构成今天放行的理由。
	Window time.Duration
}

// trustKey 信任累积的粒度：同一 Agent 对同一类 checkpoint 的历史。
// 刻意不含具体目标资源——粒度过细会导致信任永远累积不起来，
// 粒度过粗（只按 checkpoint_type）又会让 A Agent 的批准记录惠及 B Agent。
type trustKey struct {
	checkpointType string
	agentID        string
}

type trustEntry struct {
	approvals int
	updatedAt time.Time
}

// TrustScorer 累积人工批准记录并判定是否可降级为通知。
// 所有方法 nil-safe：未注入时 ShouldDowngrade 恒返回 false（等价于机制关闭）。
type TrustScorer struct {
	policy TrustPolicy

	mu      sync.Mutex
	entries map[trustKey]*trustEntry
}

// NewTrustScorer 构造信任评分器。policy.MinApprovals<=0 时机制关闭。
func NewTrustScorer(policy TrustPolicy) *TrustScorer {
	if policy.Window <= 0 {
		policy.Window = 24 * time.Hour
	}
	return &TrustScorer{policy: policy, entries: make(map[trustKey]*trustEntry)}
}

// Enabled 报告机制是否启用。
func (s *TrustScorer) Enabled() bool {
	return s != nil && s.policy.MinApprovals > 0
}

// RecordDecision 记录一次**人工**审批结果。
// 只接受人工决策——把自动放行/自动拒绝计入信任累积会形成正反馈：
// 降级产生的"通过"又反过来加固降级依据，几轮之后就没人在看了。
func (s *TrustScorer) RecordDecision(p types.HITLPrompt, approved bool) {
	if !s.Enabled() {
		return
	}
	k := trustKey{checkpointType: p.CheckpointType, agentID: p.AgentID}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !approved {
		// 任何一次人工拒绝都意味着"这类请求仍需人看"，信任立即清零。
		delete(s.entries, k)
		return
	}
	e, ok := s.entries[k]
	if !ok || time.Since(e.updatedAt) > s.policy.Window {
		s.entries[k] = &trustEntry{approvals: 1, updatedAt: time.Now()}
		return
	}
	e.approvals++
	e.updatedAt = time.Now()
}

// ShouldDowngrade 判定本次审批可否降级为通知（不阻塞、只告知）。
//
// 返回 true 仅表示"降级为通知"，绝不表示"静默放行"——调用方仍须把这次
// 操作以通知形式告知用户，并记入审计。
func (s *TrustScorer) ShouldDowngrade(p types.HITLPrompt) bool {
	if !s.Enabled() {
		return false
	}
	if !downgradeEligible(p) {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[trustKey{checkpointType: p.CheckpointType, agentID: p.AgentID}]
	if !ok || time.Since(e.updatedAt) > s.policy.Window {
		return false
	}
	return e.approvals >= s.policy.MinApprovals
}

// downgradeEligible 硬地板：哪些审批**永远**不参与自适应降级。
//
// 这组条件与 resolveTimeoutAction 的地板刻意保持一致——两处对"什么算高危"
// 必须给出同一答案，否则会出现"超时不敢自动放行、但疲劳降级放行了"的
// 荒谬组合。新增豁免必须同时改两处并走 ADR。
func downgradeEligible(p types.HITLPrompt) bool {
	// 污点：处理过外部不可信内容的请求一律人工复核，与 M13 §2.4 地板一致。
	if p.TaintLevel >= types.TaintMedium {
		return false
	}
	// 高风险等级。
	if p.RiskLevel >= 3 {
		return false
	}
	// 设备操控（电脑/浏览器）——后果不可逆且用户预期就是"每次都问"。
	if p.CheckpointType == types.CheckpointDeviceControlReview {
		return false
	}
	// L4 自我改进晋升：它自带 L3 全量回归门禁与强制冷却，本就不是高频审批，
	// 降级它没有收益，只有风险。
	if p.CheckpointType == "l4_multi_sig" {
		return false
	}
	// 无法归因到具体 Agent 的请求不参与——信任必须有明确主体。
	return p.AgentID != ""
}
