package prompt

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

func layeredFixture(t *testing.T) *PromptBuilder {
	t.Helper()
	b := NewPromptBuilder()
	// 故意乱序写入：BuildLayered 必须按层输出，与写入顺序无关。
	b.WriteUserData(taint.NewTaintedString("intent", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "t"))
	safe, err := taint.SanitizeToSafe(taint.NewTaintedString("phase-template", taint.TaintSource{OriginTaintLevel: types.TaintNone}, "t"))
	if err != nil {
		t.Fatal(err)
	}
	b.WriteInstruction(safe)
	b.WriteHistoryMessage("assistant", taint.NewTaintedString("hist", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "t"))
	b.WriteExternalCatalog("extensions", taint.NewTaintedString("ext", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "t"))
	b.WritePhaseSystem("volatile")
	b.WriteStable(types.Message{Role: "system", Content: "stable"})
	return b
}

func TestBuildLayered_OrdersByLayerNotWriteOrder(t *testing.T) {
	msgs := layeredFixture(t).BuildLayered()
	wantContains := []string{"stable", "ext", "hist", "phase-template", "volatile", "intent"}
	if len(msgs) != len(wantContains) {
		t.Fatalf("消息数 %d, want %d", len(msgs), len(wantContains))
	}
	for i, w := range wantContains {
		if !strings.Contains(msgs[i].Content, w) {
			t.Fatalf("第 %d 条应含 %q: %q", i, w, msgs[i].Content)
		}
	}
	// 角色与围栏不因排序而改变：历史保持真实角色并围栏，本轮数据 user 角色并围栏，模板为 system 无围栏。
	if msgs[2].Role != "assistant" || !strings.Contains(msgs[2].Content, "UNTRUSTED_DATA") {
		t.Fatalf("历史应保持 assistant 角色且围栏: %+v", msgs[2])
	}
	if msgs[3].Role != "system" || strings.Contains(msgs[3].Content, "UNTRUSTED_DATA") {
		t.Fatalf("阶段模板应为无围栏 system 指令: %+v", msgs[3])
	}
	if msgs[5].Role != "user" || !strings.Contains(msgs[5].Content, "UNTRUSTED_DATA") {
		t.Fatalf("本轮数据应为围栏 user 消息: %+v", msgs[5])
	}
}

// 旧 Build（供非内核调用方）次序不变，且不含只属于层账本的 L0/历史消息。
func TestBuild_LegacyZoneOrderUnchanged(t *testing.T) {
	msgs := layeredFixture(t).Build()
	// Zone 序：Immutable(phase-template) → MutableSkill(volatile) → ExternalCatalog(ext) → TaintedData(intent)
	want := []string{"phase-template", "volatile", "ext", "intent"}
	if len(msgs) != len(want) {
		t.Fatalf("Build 消息数 %d, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if !strings.Contains(msgs[i].Content, w) {
			t.Fatalf("Build 第 %d 条应含 %q: %q", i, w, msgs[i].Content)
		}
	}
}

func TestSetLayer_OverridesDefaultUntilReset(t *testing.T) {
	b := NewPromptBuilder()
	safe, err := taint.SanitizeToSafe(taint.NewTaintedString("trusted", taint.TaintSource{OriginTaintLevel: types.TaintNone}, "t"))
	if err != nil {
		t.Fatal(err)
	}
	b.SetLayer(protocol.LayerSession)
	b.WriteInstruction(safe)
	b.ResetLayer()
	b.WriteInstruction(safe) // 默认 L3
	b.WriteStable(types.Message{Role: "system", Content: "L0"})
	b.WriteHistoryMessage("user", taint.NewTaintedString("h", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "t"))
	msgs := b.BuildLayered()
	// L0, L1(trusted), L2(h), L3(trusted)
	if len(msgs) != 4 || msgs[0].Content != "L0" || !strings.Contains(msgs[2].Content, "h") {
		t.Fatalf("层次序错误: %+v", msgs)
	}
}
