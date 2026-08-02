package session

import (
	"strings"
	"testing"
)

// TestNewSessionID 从 chat/sessions_extra_test.go 迁入（A-03 Step7 死代码
// 清理）：newSessionID 本体随 A-03 Step2 迁入 ids.go，chat 包侧此前留有一份
// 现已不可达的旧实现 + 对应测试（chat 包的会话 ID 生成职责已完全转移到
// session.orchestrator.resolveSessionID），make deadcode 捕获后一并清理，
// 测试随实现迁移，不丢失覆盖率。
func TestNewSessionID(t *testing.T) {
	sID := newSessionID()
	if !strings.HasPrefix(sID, "sess_") {
		t.Errorf("expected sess_ prefix, got %s", sID)
	}
	if len(sID) != 37 {
		t.Errorf("expected length 37, got %d", len(sID))
	}
}
