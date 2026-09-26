package lifecycle

// builtinAgents 两家内置子 Agent 类型（技能 `agent:` 字段与委派目标常用）：Claude Explore / Plan
// （只读研究），Codex default / worker / explorer。项目/用户目录同名定义覆盖内置（两家规则）。
// general-purpose 不在此列——它是无角色通用 Agent，由委派执行层直接识别。
func builtinAgents() []AgentDefinition {
	const readOnlyNote = " You run in read-only mode: never modify files or state."
	return []AgentDefinition{
		{Name: "Explore", Description: "Fast read-only agent for searching and understanding codebases and documents.",
			ReadOnly: true, body: "Explore efficiently: locate relevant files, read only what you need, and report concise findings with paths." + readOnlyNote},
		{Name: "Plan", Description: "Read-only research agent that gathers context to design an implementation plan.",
			ReadOnly: true, body: "Research the task and return a concrete, step-by-step plan grounded in what you found." + readOnlyNote},
		{Name: "default", Description: "General-purpose fallback agent (Codex built-in)."},
		{Name: "worker", Description: "Execution-focused agent for implementation and fixes (Codex built-in).",
			body: "Complete the assigned change end to end, verify it, and report exactly what you changed."},
		{Name: "explorer", Description: "Read-heavy codebase exploration agent (Codex built-in).",
			ReadOnly: true, body: "Map the relevant code paths and report findings with file references." + readOnlyNote},
	}
}
