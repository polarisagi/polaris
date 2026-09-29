package agentctx

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// ADR-0105 决策一/二的前缀不变量门控（记忆路径）：
//   - 同一 StateContext 依次构造 Perceive/Plan/Reflect/Respond，L0..L2 消息序列字节一致；
//   - 相邻两回合（未跳窗）上一回合的 L0..L2 是下一回合 L0..L2 的前缀；
//   - 跳窗时 L2 首位为锚定摘要，且只在跳窗时变化；
//   - 易变层（日期/AmbientContext）位于历史之后，不打断前缀。

// ledgerL1Count 是 newLedgerCtx 构造的 L1 消息数：核心记忆 2（提示 + 1 块）+ 子 Agent 画像 1
// + 可信工作区 1 + 扩展目录 1 + 不可信工作区 1。
const ledgerL1Count = 6

func newLedgerMemory(volatile string) *mockMemory {
	return &mockMemory{
		episodic: &mockEpisodicMem{},
		working:  &mockWorkingMem{immutable: &mockImmutableCore{volatile: volatile}},
		coreBlocks: []types.CoreMemoryBlock{
			{BlockKey: "persona", Content: "core block", TaintLevel: types.TaintNone},
		},
	}
}

func newLedgerCtx(history []types.Message, intent string) *fsm.StateContext {
	return &fsm.StateContext{
		AgentID:                   "a1",
		SessionID:                 "s1",
		RawIntentTS:               taint.NewTaintedString(intent, taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		ConversationHistory:       history,
		AgentProfile:              &types.AgentProfileSpec{Name: "p", Instructions: "role", InstructionTaint: types.TaintMedium},
		WorkspaceContextTrusted:   "trusted rules",
		WorkspaceContextUntrusted: "untrusted agents.md",
		InstalledExtensionsInfo:   "ext-a, ext-b",
		TaskModel:                 &fsm.TaskModel{Goal: "goal"},
		ExecuteResult:             []byte("result"),
	}
}

func ledgerHistory(n int) []types.Message {
	h := make([]types.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		h = append(h, types.Message{Role: role, Content: fmt.Sprintf("history-%03d", i)})
	}
	return h
}

func buildAllPhases(t *testing.T, mem *mockMemory, sCtx *fsm.StateContext) map[string][]types.Message {
	t.Helper()
	ctx := context.Background()
	out := map[string][]types.Message{}
	var err error
	if out["perceive"], err = BuildPerceiveContext(ctx, mem, sCtx, nil); err != nil {
		t.Fatal(err)
	}
	if out["plan"], err = BuildPlanContext(ctx, mem, sCtx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if out["reflect"], err = BuildReflectContext(ctx, mem, sCtx); err != nil {
		t.Fatal(err)
	}
	if out["respond"], err = BuildRespondContext(ctx, mem, sCtx); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameMsg(a, b types.Message) bool {
	return a.Role == b.Role && a.Content == b.Content && a.ReasoningContent == b.ReasoningContent
}

func TestPrefixLedger_PhasesShareL0toL2(t *testing.T) {
	const histLen = 6
	prefix := 1 + ledgerL1Count + histLen
	// 易变层每次都不同（模拟日期/AmbientContext 随问题变化）：不得影响前缀。
	phases := buildAllPhases(t, newLedgerMemory("# VOLATILE CONTEXT\n日期 X"), newLedgerCtx(ledgerHistory(histLen), "本轮意图"))

	base := phases["perceive"]
	if len(base) < prefix {
		t.Fatalf("perceive 消息过短: %d", len(base))
	}
	if base[0].Content != "[Immutable Core Rule: NO HARMFUL ACT]" {
		t.Fatalf("L0 稳定核必须是第一条: %q", base[0].Content)
	}
	for name, msgs := range phases {
		if len(msgs) < prefix {
			t.Fatalf("%s 消息过短: %d", name, len(msgs))
		}
		for i := 0; i < prefix; i++ {
			if !sameMsg(msgs[i], base[i]) {
				t.Fatalf("%s 的第 %d 条与 perceive 不一致（L0..L2 必须字节一致）:\n%q\n%q", name, i, msgs[i].Content, base[i].Content)
			}
		}
		// L2 末条是最后一条历史。
		if last := msgs[prefix-1]; !strings.Contains(last.Content, "history-005") {
			t.Fatalf("%s: L2 末条应为最后一条历史: %q", name, last.Content)
		}
		// 阶段层之后才出现易变内容，且位于历史之后。
		volIdx := -1
		for i, m := range msgs {
			if strings.Contains(m.Content, "日期 X") {
				volIdx = i
			}
		}
		if volIdx < prefix {
			t.Fatalf("%s: 易变层必须位于 L2 历史之后，实际下标 %d", name, volIdx)
		}
	}
}

// 易变层变化只能影响 L3 之后：L0..L2 前缀在不同易变内容下仍逐字节相同。
func TestPrefixLedger_VolatileChangeKeepsPrefix(t *testing.T) {
	const histLen = 6
	prefix := 1 + ledgerL1Count + histLen
	a := buildAllPhases(t, newLedgerMemory("# VOLATILE CONTEXT\n日期 A / skill-a"), newLedgerCtx(ledgerHistory(histLen), "问题一"))
	b := buildAllPhases(t, newLedgerMemory("# VOLATILE CONTEXT\n日期 B / skill-b"), newLedgerCtx(ledgerHistory(histLen), "问题二"))
	for name := range a {
		for i := 0; i < prefix; i++ {
			if !sameMsg(a[name][i], b[name][i]) {
				t.Fatalf("%s: 易变层/本轮意图变化不应改变第 %d 条", name, i)
			}
		}
	}
}

// 相邻回合：历史只追加 → 上一回合的 L0..L2 是下一回合 L0..L2 的前缀。
func TestPrefixLedger_AdjacentTurnsAppendOnly(t *testing.T) {
	mem := newLedgerMemory("")
	prev := buildAllPhases(t, mem, newLedgerCtx(ledgerHistory(6), "turn-1"))
	next := buildAllPhases(t, mem, newLedgerCtx(ledgerHistory(8), "turn-2"))
	prefix := 1 + ledgerL1Count + 6
	for name := range prev {
		for i := 0; i < prefix; i++ {
			if !sameMsg(prev[name][i], next[name][i]) {
				t.Fatalf("%s: 相邻回合（未跳窗）前缀第 %d 条发生变化", name, i)
			}
		}
	}
}

// 跳窗：越过 history_max_messages 后 L2 首位是锚定摘要；摘要只在跳窗时变化。
func TestPrefixLedger_WindowJump(t *testing.T) {
	mem := newLedgerMemory("")
	l2First := func(n int) types.Message {
		msgs, err := BuildPerceiveContext(context.Background(), mem, newLedgerCtx(ledgerHistory(n), "q"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return msgs[1+ledgerL1Count]
	}
	noJump := l2First(20)
	if strings.Contains(noJump.Content, "conversation_summary") {
		t.Fatalf("未越阈值不应有锚定摘要: %q", noJump.Content)
	}
	j1 := l2First(21)
	if !strings.Contains(j1.Content, "conversation_summary") || !strings.Contains(j1.Content, "UNTRUSTED_DATA") {
		t.Fatalf("跳窗后 L2 首位应为 TaintHigh 围栏的锚定摘要: %q", j1.Content)
	}
	if j1.Role != "user" {
		t.Fatalf("锚定摘要派生自不可信历史，必须是 user 角色数据: %s", j1.Role)
	}
	for n := 22; n <= 30; n++ {
		if got := l2First(n); !sameMsg(got, j1) {
			t.Fatalf("n=%d 未跳窗，锚定摘要不应变化", n)
		}
	}
	if j2 := l2First(31); sameMsg(j2, j1) {
		t.Fatal("n=31 再次跳窗，锚定摘要应变化")
	}
}

// 每条历史消息独立围栏：旧消息字节不随新消息变化，且围栏标记只由该条内容决定。
func TestPrefixLedger_HistoryFencedPerMessage(t *testing.T) {
	mem := newLedgerMemory("")
	msgs, err := BuildPerceiveContext(context.Background(), mem, newLedgerCtx(ledgerHistory(4), "q"), nil)
	if err != nil {
		t.Fatal(err)
	}
	hist := msgs[1+ledgerL1Count : 1+ledgerL1Count+4]
	markers := map[string]bool{}
	for i, m := range hist {
		if !strings.Contains(m.Content, "=== UNTRUSTED_DATA_") || !strings.Contains(m.Content, fmt.Sprintf("history-%03d", i)) {
			t.Fatalf("历史第 %d 条应独立围栏: %q", i, m.Content)
		}
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if m.Role != want {
			t.Fatalf("历史第 %d 条应保持真实角色 %s，got %s", i, want, m.Role)
		}
		markers[strings.SplitN(m.Content, "\n", 2)[0]] = true
	}
	if len(markers) != 4 {
		t.Fatalf("围栏标记应按单条内容哈希（4 条内容 4 个标记），got %d", len(markers))
	}
}
