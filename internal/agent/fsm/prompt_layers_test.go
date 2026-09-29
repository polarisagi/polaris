package fsm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 降级路径（无记忆系统）的前缀不变量：四个阶段的 L1/L2 与记忆路径同构、彼此字节一致
// （ADR-0105 决策一/二）。无 ImmutableCore 故无 L0。
func TestPromptLayers_FallbackPhasesShareL1L2(t *testing.T) {
	hist := make([]types.Message, 0, 6)
	for i := 0; i < 6; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		hist = append(hist, types.Message{Role: role, Content: fmt.Sprintf("history-%03d", i)})
	}
	sCtx := &StateContext{
		AgentID:                   "a1",
		SessionID:                 "s1",
		RawIntentTS:               taint.NewTaintedString("本轮意图", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		ConversationHistory:       hist,
		AgentProfile:              &types.AgentProfileSpec{Name: "p", Instructions: "role", InstructionTaint: types.TaintMedium},
		WorkspaceContextTrusted:   "trusted rules",
		WorkspaceContextUntrusted: "untrusted agents.md",
		InstalledExtensionsInfo:   "ext-a, ext-b",
		TaskModel:                 &TaskModel{Goal: "goal"},
		ExecuteResult:             []byte("result"),
	}
	sm := &StateMachine{cb: &dummyContextBuilder{}}
	pCtx := protocol.StateContext{SessionID: "s1"}

	phases := map[string][]types.Message{
		"perceive": sm.promptPerceive(sCtx, pCtx),
		"plan":     sm.promptPlan(sCtx, pCtx),
		"reflect":  sm.promptReflect(sCtx, pCtx),
		"respond":  sm.promptRespond(sCtx, pCtx),
	}
	// L1：画像 + 可信工作区 + 扩展目录 + 不可信工作区 = 4；L2：6 条历史。
	const prefix = 4 + 6
	base := phases["perceive"]
	for name, msgs := range phases {
		if len(msgs) <= prefix {
			t.Fatalf("%s 消息过短: %d", name, len(msgs))
		}
		for i := 0; i < prefix; i++ {
			if msgs[i].Role != base[i].Role || msgs[i].Content != base[i].Content {
				t.Fatalf("%s 第 %d 条与 perceive 不一致:\n%q\n%q", name, i, msgs[i].Content, base[i].Content)
			}
		}
		if !strings.Contains(msgs[prefix-1].Content, "history-005") {
			t.Fatalf("%s: L2 末条应为最后一条历史: %q", name, msgs[prefix-1].Content)
		}
	}
	// 可信工作区指令在 L1（指令区，无围栏），不可信的必须围栏。
	if !strings.Contains(base[1].Content, "trusted rules") || strings.Contains(base[1].Content, "UNTRUSTED_DATA") {
		t.Fatalf("可信工作区指令应位于 L1 指令区: %q", base[1].Content)
	}
	if !strings.Contains(base[3].Content, "UNTRUSTED_DATA") || !strings.Contains(base[3].Content, "untrusted agents.md") {
		t.Fatalf("不可信工作区上下文必须 TaintHigh 围栏: %q", base[3].Content)
	}
}

// Plan 的运行期提示块（重规划新增工具/tool-hints）只能进 L3，不得改写 L1/L2。
func TestPromptLayers_PlanHintsStayInPhaseLayer(t *testing.T) {
	sCtx := &StateContext{
		RawIntentTS:         taint.NewTaintedString("x", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		ConversationHistory: []types.Message{{Role: "user", Content: "old"}},
	}
	sm := &StateMachine{cb: &dummyContextBuilder{}}
	pCtx := protocol.StateContext{SessionID: "s1"}
	before := sm.promptPlan(sCtx, pCtx)

	sm.dynamicHints = []ExtActivatedHint{{ToolName: "new_tool", Description: "desc"}}
	sm.WithToolHintProvider(&mockToolHintProvider{hint: "<tool-hints>h</tool-hints>"})
	after := sm.promptPlan(sCtx, pCtx)

	// 无 L1（无扩展/画像），L2 为 1 条历史：下标 0 必须字节相同。
	if before[0].Content != after[0].Content {
		t.Fatalf("提示块不得改写前缀: %q vs %q", before[0].Content, after[0].Content)
	}
	joined := ""
	for _, m := range after {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "new_tool") || !strings.Contains(joined, "<tool-hints>h</tool-hints>") {
		t.Fatal("提示块应出现在 Plan 请求中")
	}
}
