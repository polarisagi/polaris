package agent

import (
	"reflect"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/prompt"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// Test_inv_M4_02_EventIDDeterminism 验证 NextEventID 不依赖 wall clock。
// inv_M4_02: 同 session_id + seq → 同 Event ID，保证重放确定性。
func Test_inv_M4_02_EventIDDeterminism(t *testing.T) {
	sm1 := fsm.NewStateMachine(&dummyContextBuilder{})
	sm2 := fsm.NewStateMachine(&dummyContextBuilder{})

	id1 := sm1.NextEventID("session-abc", "perceive")
	id2 := sm2.NextEventID("session-abc", "perceive")
	if id1 != id2 {
		t.Errorf("inv_M4_02: 同 session+seq 应产生相同 ID, got %q != %q", id1, id2)
	}

	// wall clock 不影响结果
	time.Sleep(5 * time.Millisecond)
	sm3 := fsm.NewStateMachine(&dummyContextBuilder{})
	id3 := sm3.NextEventID("session-abc", "perceive")
	if id3 != id1 {
		t.Errorf("inv_M4_02: wall clock 不应影响 Event ID, got %q != %q after sleep", id3, id1)
	}

	// 不同 seq 必然不同
	id4 := sm1.NextEventID("session-abc", "plan") // eventSeq = 2
	if id4 == id1 {
		t.Error("inv_M4_02: 不同 seq 应产生不同 ID")
	}

	// 不同 session 必然不同
	sm4 := fsm.NewStateMachine(&dummyContextBuilder{})
	id5 := sm4.NextEventID("session-xyz", "perceive")
	if id5 == id1 {
		t.Error("inv_M4_02: 不同 session 应产生不同 ID")
	}
}

// Test_inv_M4_03_PromptBuilderDeterminism 验证 PromptBuilder 为纯函数——同输入→同输出。
// inv_M4_03: PromptFn 禁止依赖 wall_clock/random，保证重放时 prompt 字节一致。
func Test_inv_M4_03_PromptBuilderDeterminism(t *testing.T) {
	// 构造 SafeString（TaintLow → SanitizeByUserReview → TaintUserReviewed → SafeString）
	ts := taint.NewTaintedString("system instruction", taint.TaintSource{
		Module:           "test",
		OriginTaintLevel: types.TaintLow,
	}, "test")
	reviewed := taint.SanitizeByUserReview(ts, "test-reviewer")
	safe, err := taint.SanitizeToSafe(reviewed)
	if err != nil {
		t.Fatalf("SanitizeToSafe: %v", err)
	}

	buildOnce := func() []types.Message {
		b := prompt.NewPromptBuilder()
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
	ts := taint.NewTaintedString("data", taint.TaintSource{
		OriginTaintLevel: types.TaintLow,
	}, "")
	reviewed := taint.SanitizeByUserReview(ts, "r")
	safe, _ := taint.SanitizeToSafe(reviewed)

	b1 := prompt.NewPromptBuilder()
	b1.WriteInstruction(safe)
	msgs1 := b1.Build()

	time.Sleep(10 * time.Millisecond)

	b2 := prompt.NewPromptBuilder()
	b2.WriteInstruction(safe)
	msgs2 := b2.Build()

	if !reflect.DeepEqual(msgs1, msgs2) {
		t.Errorf("inv_M4_03: PromptBuilder 包含 wall clock 依赖（sleep 后输出不同）")
	}
}
