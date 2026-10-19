package agent

import "github.com/polarisagi/polaris/pkg/types"

// TrySteer 尝试向当前会话中正在运行的 Agent 注入插话（ADR-0115 WP2）。
// 若会话存在且 Agent 处于 AgentStateLoop 或 AgentStateLoopExec 态，则直接入队而无需占用新 Slot。
// 若回合已结束或正在收尾，返回 false，由网关按普通新回合处理（R2-12）。
func (p *Pool) TrySteer(sessionID string, text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.sessions[sessionID]
	if !ok || entry.agent == nil {
		return false
	}
	select {
	case <-entry.agent.Done():
		return false
	default:
	}
	st := entry.agent.sm.Current()
	if st != types.AgentStateLoop && st != types.AgentStateLoopExec && st != types.AgentStateLoopAwait {
		return false
	}
	return entry.agent.Steer(text)
}

// StopSession 中止指定 session 的 Agent（取消其执行并在 Pool 中释放）（R2-14）。
func (p *Pool) StopSession(sessionID string) bool {
	p.mu.Lock()
	entry, ok := p.sessions[sessionID]
	if !ok || entry.agent == nil {
		p.mu.Unlock()
		return false
	}
	ag := entry.agent
	delete(p.sessions, sessionID)
	if p.onSessionClose != nil {
		p.onSessionClose(sessionID)
	}
	p.mu.Unlock()

	ag.Interrupt(types.InterruptRequest{Action: types.InterruptAbort})
	ag.Shutdown()
	return true
}
