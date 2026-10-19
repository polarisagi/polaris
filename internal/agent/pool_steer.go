package agent

import "github.com/polarisagi/polaris/pkg/types"

// TrySteer 尝试向当前会话中正在运行的 Agent 注入插话（ADR-0115 WP2）。
// 若会话存在且 Agent 处于 AgentStateLoop 或 AgentStateLoopExec 态，则直接入队而无需占用新 Slot。
func (p *Pool) TrySteer(sessionID string, text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.sessions[sessionID]
	if !ok || entry.agent == nil {
		return false
	}
	st := entry.agent.sm.Current()
	if st != types.AgentStateLoop && st != types.AgentStateLoopExec && st != types.AgentStateLoopAwait {
		return false
	}
	return entry.agent.Steer(text)
}
