package kernel

import (
	"reflect"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// Test_inv_M4_02_EventIDDeterminism 验证 nextEventID 不依赖 wall clock。
// inv_M4_02: 同 session_id + seq → 同 Event ID，保证重放确定性。
func Test_inv_M4_02_EventIDDeterminism(t *testing.T) {
	sm1 := NewStateMachine()
	sm2 := NewStateMachine()

	id1 := sm1.nextEventID("session-abc", "perceive")
	id2 := sm2.nextEventID("session-abc", "perceive")
	if id1 != id2 {
		t.Errorf("inv_M4_02: 同 session+seq 应产生相同 ID, got %q != %q", id1, id2)
	}

	// wall clock 不影响结果
	time.Sleep(5 * time.Millisecond)
	sm3 := NewStateMachine()
	id3 := sm3.nextEventID("session-abc", "perceive")
	if id3 != id1 {
		t.Errorf("inv_M4_02: wall clock 不应影响 Event ID, got %q != %q after sleep", id3, id1)
	}

	// 不同 seq 必然不同
	id4 := sm1.nextEventID("session-abc", "plan") // eventSeq = 2
	if id4 == id1 {
		t.Error("inv_M4_02: 不同 seq 应产生不同 ID")
	}

	// 不同 session 必然不同
	sm4 := NewStateMachine()
	id5 := sm4.nextEventID("session-xyz", "perceive")
	if id5 == id1 {
		t.Error("inv_M4_02: 不同 session 应产生不同 ID")
	}
}

// Test_inv_M4_03_PromptBuilderDeterminism 验证 PromptBuilder 为纯函数——同输入→同输出。
// inv_M4_03: PromptFn 禁止依赖 wall_clock/random，保证重放时 prompt 字节一致。
func Test_inv_M4_03_PromptBuilderDeterminism(t *testing.T) {
	// 构造 SafeString（TaintLow → SanitizeByUserReview → TaintUserReviewed → SafeString）
	ts := substrate.NewTaintedString("system instruction", substrate.TaintSource{
		Module:           "test",
		OriginTaintLevel: protocol.TaintLow,
	}, "test")
	reviewed := substrate.SanitizeByUserReview(ts, "test-reviewer")
	safe, err := substrate.SanitizeToSafe(reviewed)
	if err != nil {
		t.Fatalf("SanitizeToSafe: %v", err)
	}

	buildOnce := func() []protocol.Message {
		b := NewPromptBuilder()
		b.WriteInstruction(safe)
		b.WriteSystemEnvironment("OS: linux | Arch: amd64")
		b.WriteUserInstruction(safe)
		return b.Build()
	}

	msgs1 := buildOnce()
	msgs2 := buildOnce()

	if !reflect.DeepEqual(msgs1, msgs2) {
		t.Errorf("inv_M4_03: PromptBuilder 不确定性——两次相同输入产生不同 messages:\n  got1=%v\n  got2=%v", msgs1, msgs2)
	}
}

// Test_inv_M4_03_PromptBuilderNoWallClock 验证 PromptBuilder 不依赖时间。
// inv_M4_03: wall_clock 不得进入 prompt 构建路径。
func Test_inv_M4_03_PromptBuilderNoWallClock(t *testing.T) {
	ts := substrate.NewTaintedString("data", substrate.TaintSource{
		OriginTaintLevel: protocol.TaintLow,
	}, "")
	reviewed := substrate.SanitizeByUserReview(ts, "r")
	safe, _ := substrate.SanitizeToSafe(reviewed)

	b1 := NewPromptBuilder()
	b1.WriteInstruction(safe)
	msgs1 := b1.Build()

	time.Sleep(10 * time.Millisecond)

	b2 := NewPromptBuilder()
	b2.WriteInstruction(safe)
	msgs2 := b2.Build()

	if !reflect.DeepEqual(msgs1, msgs2) {
		t.Errorf("inv_M4_03: PromptBuilder 包含 wall clock 依赖（sleep 后输出不同）")
	}
}
