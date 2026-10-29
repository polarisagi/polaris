package agentctx

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ADR-0105 决策九门控：阶段契约库并入 L0 稳定核，各阶段 L3 只写选择器。
// 用真实 store.ImmutableCore（而非 mock）驱动，才能证明 L0 里真的带着契约、且四阶段字节一致。

// realCoreMemory 在 mockMemory 之上换入真实 ImmutableCore。
type realCoreMemory struct {
	*mockMemory
	core protocol.ImmutableCore
}

func (m *realCoreMemory) ImmutableCore() protocol.ImmutableCore { return m.core }

func newRealCoreMemory(volatile string) *realCoreMemory {
	core := store.NewImmutableCore()
	core.SoulMDContent = "You are the test persona."
	core.VolatileBlock = volatile
	return &realCoreMemory{mockMemory: newLedgerMemory(""), core: core}
}

// setPhaseContractsSwitch 设置开关并在测试结束后还原全局配置。
func setPhaseContractsSwitch(t *testing.T, on bool) {
	t.Helper()
	prev := config.Get()
	var c config.Config
	if prev != nil {
		c = *prev
	} else {
		c.Thresholds = config.DefaultThresholds()
	}
	c.Thresholds.M4Kernel.PromptPhaseContractsInCore = on
	config.Update(&c)
	t.Cleanup(func() { config.Update(prev) })
}

// buildAllPhasesFacade 与 buildAllPhases 相同，但接收 MemoryFacade 以便换入真实 core。
func buildAllPhasesFacade(t *testing.T, mem protocol.MemoryFacade, sCtx *fsm.StateContext) map[string][]types.Message {
	t.Helper()
	ctx := t.Context()
	out := map[string][]types.Message{}
	var err error
	if out["PERCEIVE"], err = BuildPerceiveContext(ctx, mem, sCtx, nil); err != nil {
		t.Fatal(err)
	}
	if out["PLAN"], err = BuildPlanContext(ctx, mem, sCtx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if out["REFLECT"], err = BuildReflectContext(ctx, mem, sCtx); err != nil {
		t.Fatal(err)
	}
	if out["RESPOND"], err = BuildRespondContext(ctx, mem, sCtx); err != nil {
		t.Fatal(err)
	}
	return out
}

func templateBody(t *testing.T, name string) string {
	t.Helper()
	body, err := configs.LoadPromptTemplate(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(body)
}

func TestPhaseContracts_SwitchOn_ContractsInL0_SelectorInL3(t *testing.T) {
	setPhaseContractsSwitch(t, true)
	const histLen = 6
	const l0Len = 2 // L0 = [阶段契约段, 可变稳定核]（ADR-0105 决策九，WP10 起契约在最前且独立成消息）
	prefix := l0Len + ledgerL1Count + histLen
	phases := buildAllPhasesFacade(t, newRealCoreMemory("日期 X"), newLedgerCtx(ledgerHistory(histLen), "本轮意图"))

	base := phases["PERCEIVE"]
	l0 := base[0].Content
	if !strings.HasPrefix(l0, "# PHASE CONTRACTS") || l0 != configs.PhaseContractsSection() {
		t.Fatalf("L0[0] 应恰为阶段契约段常量")
	}
	if strings.Contains(base[1].Content, "# PHASE CONTRACTS") || base[1].Role != "system" {
		t.Fatalf("L0[1] 是可变稳定核，不应含契约段")
	}
	if !base[0].CacheBreakpoint || !base[1].CacheBreakpoint {
		t.Fatalf("L0 两条消息的末尾都应是缓存断点（契约段可跨会话共享）")
	}
	// 四段标题按固定顺序出现。
	last := -1
	for _, pc := range configs.PhaseContracts() {
		idx := strings.Index(l0, configs.PhaseContractHeading(pc.Phase))
		if idx < 0 || idx <= last {
			t.Fatalf("契约标题 %q 缺失或顺序错误（idx=%d last=%d）", pc.Phase, idx, last)
		}
		last = idx
		if !strings.Contains(l0, templateBody(t, pc.Template)) {
			t.Fatalf("L0 应含 %s 的完整契约正文", pc.Template)
		}
	}

	for name, msgs := range phases {
		// L0..L2 四阶段字节一致（WP1 前缀门控在契约并入后仍成立）。
		for i := 0; i < prefix; i++ {
			if !sameMsg(msgs[i], base[i]) {
				t.Fatalf("%s 的第 %d 条与 PERCEIVE 不一致", name, i)
			}
		}
		if !strings.Contains(msgs[prefix-1].Content, "history-005") {
			t.Fatalf("%s: L0=2 条时 L2 末条应位于下标 %d: %q", name, prefix-1, msgs[prefix-1].Content)
		}
		// L3：只有选择器，没有完整模板；选择器点名本阶段。
		selector := ""
		for i, m := range msgs {
			if i == 0 {
				continue
			}
			for _, pc := range configs.PhaseContracts() {
				if strings.Contains(m.Content, templateBody(t, pc.Template)) {
					t.Fatalf("%s: 第 %d 条仍含完整模板 %s（契约应只在 L0）", name, i, pc.Template)
				}
			}
			if strings.Contains(m.Content, "# ACTIVE PHASE:") {
				selector = m.Content
			}
		}
		if selector == "" || !strings.Contains(selector, "# ACTIVE PHASE: "+name) {
			t.Fatalf("%s: 缺少指向本阶段的选择器: %q", name, selector)
		}
		if len(selector) > 400 {
			t.Fatalf("%s: 选择器应为短文本，got %d 字节", name, len(selector))
		}
	}
	// respond_reminder 仍在末尾。
	rs := phases["RESPOND"]
	if tail := rs[len(rs)-1].Content; !strings.Contains(tail, strings.TrimSpace(templateBody(t, "kernel/respond_reminder.md"))) {
		t.Fatalf("respond_reminder 应保持在末尾: %q", tail)
	}
}

func TestPhaseContracts_SwitchOff_FullTemplateInL3(t *testing.T) {
	setPhaseContractsSwitch(t, false)
	phases := buildAllPhasesFacade(t, newRealCoreMemory("日期 X"), newLedgerCtx(ledgerHistory(6), "本轮意图"))
	tmpl := map[string]string{
		"PERCEIVE": "kernel/perceive.md", "PLAN": "kernel/plan.md",
		"REFLECT": "kernel/reflect.md", "RESPOND": "kernel/respond.md",
	}
	for name, msgs := range phases {
		if strings.Contains(msgs[0].Content, "# PHASE CONTRACTS") {
			t.Fatalf("%s: 开关关闭时 L0 不应含契约段", name)
		}
		body := templateBody(t, tmpl[name])
		found := false
		for i, m := range msgs {
			if i > 0 && strings.Contains(m.Content, body) {
				found = true
			}
			if strings.Contains(m.Content, "# ACTIVE PHASE:") {
				t.Fatalf("%s: 开关关闭时不应出现选择器", name)
			}
		}
		if !found {
			t.Fatalf("%s: 开关关闭时 L3 应写完整模板", name)
		}
	}
}

// ImmutableCore 不具备契约能力（如 mock）或无 ImmutableCore 时，即使开关开启也写完整模板，
// 否则 L0 没有契约而 L3 只有选择器，模型将看不到输出格式。
func TestPhaseContracts_NoCapableCoreFallsBackToFullTemplate(t *testing.T) {
	setPhaseContractsSwitch(t, true)
	phases := buildAllPhases(t, newLedgerMemory(""), newLedgerCtx(ledgerHistory(4), "q"))
	for name, msgs := range phases {
		joined := ""
		for _, m := range msgs {
			joined += m.Content + "\n"
		}
		if strings.Contains(joined, "# ACTIVE PHASE:") {
			t.Fatalf("%s: mock core 不具备契约能力，不应写选择器", name)
		}
	}
	if !strings.Contains(joinedContents(phases["perceive"]), "TASK PERCEPTION") {
		t.Fatal("perceive 应回退为完整模板")
	}
}

func joinedContents(msgs []types.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}
