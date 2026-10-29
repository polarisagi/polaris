package agentctx

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/pkg/types"
)

// B4：agentic 循环的系统提示不得混入 staged 阶段契约库。契约导语要求模型寻找末尾的
// "# ACTIVE PHASE" 选择器，而循环模式没有阶段也不写选择器；2026-10-20 实测模型在思考链里
// 反复困惑"该输出 TaskModel JSON？"，既误导又每轮白付约 2.5K token。
func TestBuildAgenticPrompt_NoStagedPhaseContracts(t *testing.T) {
	setPhaseContractsSwitch(t, true) // 开关开启是最容易泄漏契约的配置
	mem := newRealCoreMemory("日期 X")
	sCtx := newLedgerCtx(ledgerHistory(2), "请帮我搜索")

	msgs, err := fsm.BuildAgenticPrompt(t.Context(), mem, sCtx, nil, "[usage] tokens=1")
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, m := range msgs {
		all.WriteString(m.Content)
		all.WriteString("\n")
	}
	got := all.String()
	for _, banned := range []string{"# PHASE CONTRACTS", "# ACTIVE PHASE", "## PHASE: ", "TaskModel", "Perceive"} {
		if strings.Contains(got, banned) {
			t.Errorf("agentic 提示词不应含 staged 阶段内容 %q", banned)
		}
	}
	if !strings.Contains(got, "Loop mode:") {
		t.Error("agentic 提示词必须含循环模式指令 kernel/loop.md")
	}

	// 缓存前缀稳定（ADR-0105）：同一会话相邻两轮，L0..L3 字节一致，仅末尾易变块可变。
	msgs2, _ := fsm.BuildAgenticPrompt(t.Context(), mem, sCtx, nil, "[usage] tokens=999")
	if len(msgs) != len(msgs2) {
		t.Fatalf("消息条数不稳定 %d vs %d", len(msgs), len(msgs2))
	}
	for i := 0; i < len(msgs)-1; i++ {
		if !sameMsg(msgs[i], msgs2[i]) {
			t.Fatalf("前缀第 %d 条跨轮不一致", i)
		}
	}

	// 改前/改后的系统提示规模（go test -v 可见）：改前 L0 另带契约段。
	tok := llmparent.NewTiktokenTokenizer("deepseek-v4")
	sys := func(ms []types.Message) (n int) {
		for _, m := range ms {
			if m.Role == "system" {
				n += tok.CountTokens(m.Content)
			}
		}
		return n
	}
	after := sys(msgs)
	contracts := tok.CountTokens(configs.PhaseContractsSection())
	t.Logf("agentic system prompt tokens: 改前=%d（含契约段 %d） 改后=%d", after+contracts, contracts, after)
}
