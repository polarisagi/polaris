package configs

import (
	"strings"
	"sync"
)

// 阶段契约库（ADR-0105 决策九）：四阶段输出契约的部署期常量渲染。
//
// 为什么放在 configs：契约正文就是 prompts/kernel/<phase>.md 这四个嵌入模板，
// 渲染入口（store.ImmutableCore 的 L0）与选择器文本（fsm/agent/context 的 L3）
// 两侧必须使用同一套阶段名/标题，放在 configs 避免 store 与 fsm 各自硬编码后漂移。

// PhaseContract 描述契约库中的一个阶段：标题用名 + 对应嵌入模板。
type PhaseContract struct {
	Phase    string // 标题用名（大写），如 "PLAN"
	Template string // LoadPromptTemplate 的 name，如 "kernel/plan.md"
}

// PhaseContracts 返回契约段的固定渲染顺序：PERCEIVE → PLAN → REFLECT → RESPOND（即状态机主序）。
// 顺序属于字节稳定性契约的一部分，改动会使所有会话的 L0 前缀缓存一次性失效。
// 用函数返回新切片而非导出包级变量：调用方无法就地改序，字节稳定性不依赖调用方自觉。
func PhaseContracts() []PhaseContract {
	return []PhaseContract{
		{Phase: "PERCEIVE", Template: "kernel/perceive.md"},
		{Phase: "PLAN", Template: "kernel/plan.md"},
		{Phase: "REFLECT", Template: "kernel/reflect.md"},
		{Phase: "RESPOND", Template: "kernel/respond.md"},
	}
}

// PhaseContractsTitle 契约段总标题。
const PhaseContractsTitle = "# PHASE CONTRACTS"

// PhaseContractHeading 返回某阶段契约的二级标题，选择器按此名称指向契约。
func PhaseContractHeading(phase string) string {
	return "## PHASE: " + phase
}

// PhaseOfTemplate 由阶段模板名（如 "kernel/plan.md"）反查阶段标题用名；不在契约库中返回 false。
func PhaseOfTemplate(template string) (string, bool) {
	for _, pc := range PhaseContracts() {
		if pc.Template == template {
			return pc.Phase, true
		}
	}
	return "", false
}

// PhaseSelector 返回各阶段 L3 的选择器文本：只点名当前阶段，契约正文在 L0。
// 文本为纯常量（仅随阶段名变化），不含任何会话/时间变量。
func PhaseSelector(phase string) string {
	return "# ACTIVE PHASE: " + phase + "\n" +
		"Follow ONLY the \"PHASE: " + phase + "\" contract in the system prompt above. " +
		"Ignore the other phase contracts for this response."
}

// phaseContractsPreamble 契约段导语：告诉模型契约库的用法，使"只遵循当前阶段"
// 在 L0 内自洽（L3 选择器只是再点一次名）。
const phaseContractsPreamble = "Every response you produce belongs to exactly one phase of the cognitive loop. " +
	"The contracts below define the output format of each phase. " +
	"The active phase is named at the end of the conversation (\"# ACTIVE PHASE\"); " +
	"follow only that contract and ignore the others."

// PhaseContractsSection 返回渲染好的 "# PHASE CONTRACTS" 段（进程内缓存，字节恒定）。
//
// 任一契约模板读取失败（构建缺陷）时返回空串，调用方据此整体回退到 L3 全文模板：
// 半截契约库会让选择器指向不存在的契约，比不并入更糟。模板不依赖任何模板参数
// （无 {{ }}），故以 nil 数据加载，结果即原文。
func PhaseContractsSection() string { return phaseContractsSection() }

//nolint:gochecknoglobals // sync.OnceValue 懒加载只读常量文本，无可变状态；字节恒定是本段的设计目标
var phaseContractsSection = sync.OnceValue(func() string {
	var sb strings.Builder
	sb.WriteString(PhaseContractsTitle)
	sb.WriteString("\n")
	sb.WriteString(phaseContractsPreamble)
	for _, pc := range PhaseContracts() {
		body, err := LoadPromptTemplate(pc.Template, nil)
		if err != nil || strings.TrimSpace(body) == "" {
			return "" // 留空串：整体回退
		}
		sb.WriteString("\n\n")
		sb.WriteString(PhaseContractHeading(pc.Phase))
		sb.WriteString("\n")
		sb.WriteString(strings.TrimSpace(body))
	}
	return sb.String()
})
