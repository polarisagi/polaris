package agent

import "testing"

// TestNewAgent_SessionIDPopulated 2026-07-14 回归防护：NewAgent 构造的
// sCtx.SessionID 此前从未被赋值，导致 events:session:{id}: 事件流全部塌缩进
// 空字符串 key 前缀（生产环境唯一影响面）。修复后应等于传入的 id（与
// chat_sessions.id / AgentID 同源），供 WriteStateTransEvent 等写路径使用。
func TestNewAgent_SessionIDPopulated(t *testing.T) {
	a := NewAgent("session-xyz", nil, nil, nil)
	if a.sCtx.SessionID != "session-xyz" {
		t.Errorf("expected sCtx.SessionID = %q, got %q", "session-xyz", a.sCtx.SessionID)
	}
	if a.sCtx.AgentID != a.sCtx.SessionID {
		t.Errorf("expected AgentID and SessionID to share the same id source, got AgentID=%q SessionID=%q", a.sCtx.AgentID, a.sCtx.SessionID)
	}
}

func TestNewAgentWithDefaults_SessionIDPopulated(t *testing.T) {
	a := NewAgentWithDefaults("pool-session-1")
	if a.sCtx.SessionID != "pool-session-1" {
		t.Errorf("expected sCtx.SessionID = %q, got %q", "pool-session-1", a.sCtx.SessionID)
	}
}
