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

// 契约段是部署期常量：独立的第一条 system 消息，与会话字段（易变层、身份、自定义指令、画像、工具名）无关，
// 任意两个 core 的 L0[0] 字节一致——跨会话、跨用户共享最大的稳定块。
func TestPhaseContracts_ByteStableAndSessionIndependent(t *testing.T) {
	a := newContractCore()
	a.VolatileBlock = "当前日期：2026-01-01"
	a.AmbientContext = "skill A"
	b := newContractCore()
	b.SoulMDContent = "another persona"
	b.CustomInstructions = "be terse"
	b.BuiltinTools = "read_file"
	b.UserProfile = "profile of someone else"
	b.UserPreferences["language"] = "en"
	b.VolatileBlock = "当前日期：2030-12-31"
	b.AmbientContext = "skill B"

	section := configs.PhaseContractsSection()
	ma, mb := a.StableMessagesWithContracts(), b.StableMessagesWithContracts()
	if len(ma) != 2 || len(mb) != 2 {
		t.Fatalf("L0 应为 [契约段, 可变稳定核] 两条，got %d/%d", len(ma), len(mb))
	}
	if ma[0].Content != section || mb[0].Content != section || ma[0].Role != "system" {
		t.Fatal("L0[0] 必须恰为 configs.PhaseContractsSection() 常量，与任何会话字段无关")
	}
	if ma[1].Content == mb[1].Content {
		t.Fatal("可变稳定核应随身份/指令/工具/画像变化（本断言保证上一条不是空测）")
	}
	// 多次渲染字节一致；易变层字段不进稳定层。
	again := a.StableMessagesWithContracts()
	c := newContractCore()
	c.VolatileBlock = "另一天"
	if again[1].Content != ma[1].Content || c.StableMessagesWithContracts()[1].Content != ma[1].Content {
		t.Fatal("同一 core 重复渲染、或仅易变字段不同的 core，可变稳定核应字节一致")
	}
	// 契约段不含任何时间/会话变量。
	for _, bad := range []string{"当前日期", "VOLATILE", "{{"} {
		if strings.Contains(section, bad) {
			t.Fatalf("契约段不应含变量痕迹 %q", bad)
		}
	}
}

// L0 可变稳定核按变化频率升序：配置期（身份/引导/自定义指令/平台/操作指令）→ 安装期（工具名/扩展）
// → 会话间演化（画像/偏好）。最稳定的在最前，任一处变化只使其后的内容失配。
func TestStableMessage_OrderedByVolatility(t *testing.T) {
	ic := newContractCore()
	ic.SoulMDContent = "MARK-SOUL"
	ic.ModelGuidance = "MARK-GUIDANCE"
	ic.CustomInstructions = "MARK-CUSTOM"
	ic.PlatformHint = "MARK-PLATFORM"
	ic.OperationalDirectives = "MARK-OPS"
	ic.BuiltinTools = "MARK-TOOLS"
	ic.InstalledPlugins = "MARK-PLUGINS"
	ic.UserProfile = "MARK-PROFILE"
	ic.UserPreferences["k"] = "MARK-PREF"
	content := ic.StableMessage().Content
	last := -1
	for _, mk := range []string{"MARK-SOUL", "MARK-GUIDANCE", "MARK-CUSTOM", "MARK-PLATFORM", "MARK-OPS", "MARK-TOOLS", "MARK-PLUGINS", "MARK-PROFILE", "MARK-PREF"} {
		i := strings.Index(content, mk)
		if i < 0 || i <= last {
			t.Fatalf("%s 缺失或次序违反稳定度升序（idx=%d last=%d）", mk, i, last)
		}
		last = i
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

// 截断必须优先保护契约段：自定义指令撑爆上限时，被截的是可变稳定核的尾部（最易变的画像/偏好先被牺牲），
// 契约段作为独立消息完整保留、不计入 maxSystemPromptBytes；身份等配置期内容在头部，保留。
func TestPhaseContracts_TruncationProtectsContracts(t *testing.T) {
	section := configs.PhaseContractsSection()
	huge := strings.Repeat("user instruction line.\n\n", 4000) // ≈ 92KB

	cases := map[string]*ImmutableCore{}
	ic := newContractCore()
	ic.SoulMDContent = "IDENTITY-MARKER"
	ic.CustomInstructions = huge
	ic.UserProfile = "PROFILE-MARKER"
	cases["custom_instructions"] = ic
	tpl := newContractCore()
	tpl.SystemPromptTemplate = huge
	cases["template"] = tpl

	for name, core := range cases {
		msgs := core.StableMessagesWithContracts()
		if len(msgs) != 2 || msgs[0].Content != section {
			t.Fatalf("%s: 契约段必须完整保留为第一条消息", name)
		}
		variable := msgs[1].Content
		if !strings.Contains(variable, "[...系统提示词已截断]") {
			t.Fatalf("%s: 可变部分应被截断并带标记", name)
		}
		// 可变部分受 maxSystemPromptBytes 约束（契约段是另一条消息，不占该上限）。
		if len(variable) > maxSystemPromptBytes+len("\n\n[...系统提示词已截断]") {
			t.Fatalf("%s: 可变部分超出上限: %d", name, len(variable))
		}
		if name == "custom_instructions" {
			if strings.Contains(variable, "PROFILE-MARKER") {
				t.Fatal("超长自定义指令下尾部画像应被截掉")
			}
			if !strings.Contains(variable, "IDENTITY-MARKER") {
				t.Fatal("截断从尾部开始，头部的身份必须保留")
			}
		}
		// 不含契约的路径行为不变：同样截断、无契约。
		noC := core.StableMessage().Content
		if strings.Contains(noC, "# PHASE CONTRACTS") || !strings.Contains(noC, "[...系统提示词已截断]") {
			t.Fatalf("%s: StableMessage 应截断且不含契约", name)
		}
	}
}
