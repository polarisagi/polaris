package token

import (
	"sync"
	"time"
)

// ExemptionVault 按 AgentID 保存 HITL 审批铸造的 TaintExemptionToken（M04 §3
// TaintBlocked→HITL 审批→颁发豁免令牌 转义路径）。
//
// 2026-07-14 补齐：铸造点 automation/hitl.GatewayImpl.Respond 此前只有
// `// TODO(Task 8): Insert token into vault or blackboard` 占位注释，令牌铸造后
// 无处存放，下一次工具执行也无从查询——即便铸造成功，转义路径依然形同虚设。
// 本 Vault 是该 TODO 的落地实现：进程级单例，按 AgentID 索引（一个 Agent 会话
// 在同一时间只应有一个待处理的豁免上下文，覆盖写符合"最新审批结果生效"的直觉），
// goroutine-safe，惰性过期清理（Lookup 命中过期项时顺手删除）。
type ExemptionVault struct {
	mu     sync.RWMutex
	tokens map[string]*TaintExemptionToken // agentID -> token
}

// NewExemptionVault 构造空的豁免令牌存储。
func NewExemptionVault() *ExemptionVault {
	return &ExemptionVault{tokens: make(map[string]*TaintExemptionToken)}
}

// Store 保存/覆盖某 AgentID 的豁免令牌。agentID 为空或 tok 为 nil 时忽略。
func (v *ExemptionVault) Store(agentID string, tok *TaintExemptionToken) {
	if agentID == "" || tok == nil {
		return
	}
	v.mu.Lock()
	v.tokens[agentID] = tok
	v.mu.Unlock()
}

// Lookup 返回某 AgentID 当前未过期的豁免令牌；不存在或已过期返回 nil
// （过期项顺带从存储中移除，避免长期运行下的内存堆积）。
func (v *ExemptionVault) Lookup(agentID string) *TaintExemptionToken {
	if agentID == "" {
		return nil
	}
	v.mu.RLock()
	tok, ok := v.tokens[agentID]
	v.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(tok.ExpiresAt) {
		v.mu.Lock()
		delete(v.tokens, agentID)
		v.mu.Unlock()
		return nil
	}
	return tok
}

// IsReviewed 实现 protocol.TaintReviewChecker：判断某 AgentID 当前是否持有对
// 指定 content 内容哈希匹配、未过期的豁免令牌（2026-07-14 新增，供
// internal/execute/dag.validateTaintGate 的 SanitizeByUserReview 触发点复用——
// M04 §3 HITL 审批→颁发豁免令牌这条转义路径此前只服务网络出口检查
// [checkTaintEgress]，S_VALIDATE 阶段的 TaintHigh 阻断同样需要"人工已复核"
// 判据来源，复用同一份 Vault 而非另建一套存储，避免审批语义割裂）。
func (v *ExemptionVault) IsReviewed(agentID string, content []byte) bool {
	return v.Lookup(agentID).Valid(content)
}
