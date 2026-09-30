package store

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/pkg/types"
)

// ADR-0105 决策九：阶段契约段渲染、字节稳定、截断保护、非内核路径不含契约。

func newContractCore() *ImmutableCore {
	ic := NewImmutableCore()
	ic.SoulMDContent = "persona"
	return ic
}

func TestPhaseContracts_SectionOrderAndContent(t *testing.T) {
	section := configs.PhaseContractsSection()
	if !strings.HasPrefix(section, "# PHASE CONTRACTS\n") {
		t.Fatalf("契约段应以总标题开头: %q", section[:40])
	}
	last := -1
	for _, pc := range configs.PhaseContracts() {
		idx := strings.Index(section, configs.PhaseContractHeading(pc.Phase)+"\n")
		if idx < 0 || idx <= last {
			t.Fatalf("%s 标题缺失或顺序错误", pc.Phase)
		}
		last = idx
		body, err := configs.LoadPromptTemplate(pc.Template, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(section, strings.TrimSpace(body)) {
			t.Fatalf("契约段应含 %s 正文", pc.Template)
		}
	}
	// 固定顺序 PERCEIVE → PLAN → REFLECT → RESPOND。
	want := []string{"PERCEIVE", "PLAN", "REFLECT", "RESPOND"}
	for i, pc := range configs.PhaseContracts() {
		if pc.Phase != want[i] {
			t.Fatalf("顺序第 %d 位应为 %s，got %s", i, want[i], pc.Phase)
		}
	}
}

// 契约段是部署期常量：多次渲染字节一致，且与会话字段（易变层、身份、自定义指令）无关。
func TestPhaseContracts_ByteStableAndSessionIndependent(t *testing.T) {
	a := newContractCore()
	a.VolatileBlock = "当前日期：2026-01-01"
	a.AmbientContext = "skill A"
	b := newContractCore()
	b.SoulMDContent = "another persona"
	b.CustomInstructions = "be terse"
	b.VolatileBlock = "当前日期：2030-12-31"
	b.AmbientContext = "skill B"

	first, second := a.StableMessageWithContracts().Content, a.StableMessageWithContracts().Content
	if first != second {
		t.Fatal("同一 core 两次渲染应字节一致")
	}
	// 易变字段不进稳定层：仅易变字段不同的 core，L0 字节一致。
	c := newContractCore()
	c.VolatileBlock = "另一天"
	if a.StableMessageWithContracts().Content != c.StableMessageWithContracts().Content {
		t.Fatal("易变层字段不应影响含契约的 L0")
	}
	// 身份/自定义指令不同：契约段后缀（自总标题起）必须逐字节相同。
	section := configs.PhaseContractsSection()
	for _, ic := range []*ImmutableCore{a, b} {
		if got := ic.StableMessageWithContracts().Content; !strings.HasSuffix(got, "\n\n"+section) {
			t.Fatal("L0 应以不变的契约段结尾")
		}
	}
	// 契约段不含任何时间/会话变量。
	for _, bad := range []string{"当前日期", "VOLATILE", "{{"} {
		if strings.Contains(section, bad) {
			t.Fatalf("契约段不应含变量痕迹 %q", bad)
		}
	}
}

// 非内核路径（PrependToMessages/StableMessage）不含契约段，避免白付 token。
func TestPhaseContracts_NonKernelPathsExcluded(t *testing.T) {
	ic := newContractCore()
	if strings.Contains(ic.StableMessage().Content, "# PHASE CONTRACTS") {
		t.Fatal("StableMessage 不应含契约段")
	}
	msgs := ic.PrependToMessages([]types.Message{{Role: "user", Content: "hi"}})
	for _, m := range msgs {
		if strings.Contains(m.Content, "# PHASE CONTRACTS") {
			t.Fatal("PrependToMessages 不应含契约段")
		}
	}
	if !ic.HasPhaseContracts() {
		t.Fatal("嵌入模板齐全时应具备契约能力")
	}
}

// 截断必须优先保护契约段：自定义指令撑爆上限时，被截的是可变部分，契约段完整保留。
func TestPhaseContracts_TruncationProtectsContracts(t *testing.T) {
	section := configs.PhaseContractsSection()
	huge := strings.Repeat("user instruction line.\n\n", 4000) // ≈ 92KB

	cases := map[string]*ImmutableCore{}
	ic := newContractCore()
	ic.CustomInstructions = huge
	ic.UserProfile = "PROFILE-MARKER"
	cases["custom_instructions"] = ic
	tpl := newContractCore()
	tpl.SystemPromptTemplate = huge
	cases["template"] = tpl

	for name, core := range cases {
		withC := core.StableMessageWithContracts().Content
		if !strings.HasSuffix(withC, "\n\n"+section) {
			t.Fatalf("%s: 契约段必须完整保留在末尾", name)
		}
		if !strings.Contains(withC, "[...系统提示词已截断]") {
			t.Fatalf("%s: 可变部分应被截断并带标记", name)
		}
		// 可变部分仍受 maxSystemPromptBytes 约束（契约段在其外追加）。
		variable := strings.TrimSuffix(withC, "\n\n"+section)
		if len(variable) > maxSystemPromptBytes+len("\n\n[...系统提示词已截断]") {
			t.Fatalf("%s: 可变部分超出上限: %d", name, len(variable))
		}
		// 画像在尾部，先于契约被牺牲。
		if name == "custom_instructions" && strings.Contains(withC, "PROFILE-MARKER") {
			t.Fatal("超长自定义指令下尾部画像应被截掉")
		}
		// 不含契约的路径行为不变：同样截断、无契约。
		noC := core.StableMessage().Content
		if strings.Contains(noC, "# PHASE CONTRACTS") || !strings.Contains(noC, "[...系统提示词已截断]") {
			t.Fatalf("%s: StableMessage 应截断且不含契约", name)
		}
	}
}
