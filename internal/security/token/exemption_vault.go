package token

import (
	"sync"
	"time"
)

// maxTokensPerAgent 单个 Agent 同时持有的豁免令牌上限。一份计划里需人工复核的
// 节点数受 DAG 规模约束，超出按"最旧先淘汰"——被淘汰的节点会再次触发审批，
// fail-closed，不会因此放行未复核内容。
const maxTokensPerAgent = 16

// ExemptionVault 按 AgentID 保存 HITL 审批铸造的 TaintExemptionToken（M04 §3
// TaintBlocked→HITL 审批→颁发豁免令牌 转义路径）。
//
// 2026-07-14 补齐：铸造点 automation/hitl.GatewayImpl.Respond 此前只有
// 历史：`// TODO(Task 8): Insert token into vault or blackboard` 占位注释，令牌铸造后
// 无处存放，下一次工具执行也无从查询——即便铸造成功，转义路径依然形同虚设。
// 本 Vault 是该 TODO 的落地实现：进程级单例，按 AgentID 索引，goroutine-safe，
// 惰性过期清理。
//
// 2026-09-25 修订：由"每 Agent 一枚、覆盖写"改为"每 Agent 多枚、按内容哈希匹配"。
// S_VALIDATE 的 TaintGate 人工复核接入对话后，一份计划可能有多个节点各自送审；
// 覆盖写会让第二个节点的批准冲掉第一个，重新校验时第一个节点再次被拦，审批永不收敛。
type ExemptionVault struct {
	mu     sync.Mutex
	tokens map[string][]*TaintExemptionToken // agentID -> tokens
}

// NewExemptionVault 构造空的豁免令牌存储。
func NewExemptionVault() *ExemptionVault {
	return &ExemptionVault{tokens: make(map[string][]*TaintExemptionToken)}
}

// Store 追加某 AgentID 的豁免令牌。agentID 为空或 tok 为 nil 时忽略。
func (v *ExemptionVault) Store(agentID string, tok *TaintExemptionToken) {
	if agentID == "" || tok == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	live := v.pruneLocked(agentID)
	live = append(live, tok)
	if n := len(live); n > maxTokensPerAgent {
		live = live[n-maxTokensPerAgent:]
	}
	v.tokens[agentID] = live
}

// Lookup 返回某 AgentID 持有的、对 content 内容哈希匹配且未过期的豁免令牌；
// 无匹配返回 nil（过期项顺带移除，避免长期运行下的内存堆积）。
func (v *ExemptionVault) Lookup(agentID string, content []byte) *TaintExemptionToken {
	if agentID == "" {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, tok := range v.pruneLocked(agentID) {
		if tok.Valid(content) {
			return tok
		}
	}
	return nil
}

// pruneLocked 剔除 agentID 下的过期令牌并返回存活列表；调用方须持有 v.mu。
func (v *ExemptionVault) pruneLocked(agentID string) []*TaintExemptionToken {
	now := time.Now()
	old := v.tokens[agentID]
	live := old[:0]
	for _, tok := range old {
		if now.Before(tok.ExpiresAt) {
			live = append(live, tok)
		}
	}
	if len(live) == 0 {
		delete(v.tokens, agentID)
		return nil
	}
	v.tokens[agentID] = live
	return live
}

// IsReviewed 实现 protocol.TaintReviewChecker：判断某 AgentID 当前是否持有对
// 指定 content 内容哈希匹配、未过期的豁免令牌（2026-07-14 新增，供
// internal/execute/dag.validateTaintGate 的 SanitizeByUserReview 触发点复用——
// M04 §3 HITL 审批→颁发豁免令牌这条转义路径此前只服务网络出口检查
// [checkTaintEgress]，S_VALIDATE 阶段的 TaintHigh 阻断同样需要"人工已复核"
// 判据来源，复用同一份 Vault 而非另建一套存储，避免审批语义割裂）。
func (v *ExemptionVault) IsReviewed(agentID string, content []byte) bool {
	return v.Lookup(agentID, content) != nil
}
