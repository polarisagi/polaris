package pluginspec

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// 子 Agent 定义的两种标准格式（ADR-0103 决策三）：Claude agents/*.md（YAML frontmatter + 正文
// 即系统提示）与 Codex agents/*.toml（developer_instructions）。插件内只有 Claude 格式；
// 用户/项目 agents 目录两种格式并存。
const (
	AgentFormatClaude = "claude"
	AgentFormatCodex  = "codex"

	RuleAgentNotApplied = "agent.field_not_applied"
	RuleCodexAgentParse = "codex.agent.parse"
)

const (
	reasonProviderRouting = "model selection follows Polaris per-phase model pools (ADR-0101); the agent inherits them"
	reasonPluginAgentSec  = "ignored for plugin agents (Claude security rule: plugins cannot attach hooks, MCP servers or permission modes to subagents)"
	reasonNoAgentSurface  = "no equivalent in Polaris delegated subagent execution"
)

// claudeAgentNotApplied Claude agent frontmatter 中已解析但不生效的字段 → 原因。
func claudeAgentNotApplied(inPlugin bool) map[string]string {
	m := map[string]string{
		"model":         reasonProviderRouting,
		"effort":        reasonProviderRouting,
		"background":    "delegation already runs asynchronously; the parent resumes when the subagent finishes",
		"isolation":     reasonNoAgentSurface,
		"memory":        "subagents share the delegating session's memory namespace (GD-14-001)",
		"initialPrompt": "applies only when the agent runs as the main session agent",
		"mcpServers":    "subagent-scoped MCP servers are not supported; configured connectors are available to every agent",
		"hooks":         "subagent-scoped hooks are not supported; use hooks.json SubagentStart / SubagentStop",
	}
	if inPlugin {
		m["mcpServers"], m["hooks"], m["permissionMode"] = reasonPluginAgentSec, reasonPluginAgentSec, reasonPluginAgentSec
	}
	return m
}

// ParseAgentFile 解析一个 Claude 格式子 Agent 文件（插件 agents/ 与 agents 目录共用）。
// inPlugin 决定插件受限字段的处置；返回 ok=false 表示不可加载（诊断中有 error）。
func ParseAgentFile(file, defaultName string, inPlugin bool) (AgentFile, []Diagnostic, bool) {
	var ds diagnostics
	out := appendAgent(nil, file, defaultName, inPlugin, &ds)
	if len(out) == 0 {
		return AgentFile{}, ds, false
	}
	return out[0], ds, true
}

// ListAgentDir 读取用户/项目 agents 目录：*.md（Claude）与 *.toml（Codex）。同名时 .md 优先
// 并记录告警；子目录不递归（两家的用户级 agents 目录均为平铺）。
func ListAgentDir(dir string) ([]AgentFile, []Diagnostic) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // 目录不存在是常态
	}
	var ds diagnostics
	byName := map[string]AgentFile{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		file := filepath.Join(dir, e.Name())
		var parsed []AgentFile
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".md":
			parsed = appendAgent(nil, file, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())), false, &ds)
		case ".toml":
			parsed = appendCodexAgent(nil, file, &ds)
		}
		for _, a := range parsed {
			keep, drop := a, AgentFile{}
			if prev, dup := byName[a.Name]; dup {
				keep, drop = prev, a
				if a.Format == AgentFormatClaude && prev.Format != AgentFormatClaude {
					keep, drop = a, prev
				}
				ds.warnf("agent", drop.File, RuleAgentParse, "agent %q already defined by %s; ignored", a.Name, keep.File)
			}
			byName[a.Name] = keep
		}
	}
	out := make([]AgentFile, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, ds
}

// codexAgentWire Codex agents/*.toml（developers.openai.com/codex/subagents）。其余键为
// config.toml 覆盖项，按"不适用"告警。
type codexAgentWire struct {
	Name                  string   `toml:"name"`
	Description           string   `toml:"description"`
	DeveloperInstructions string   `toml:"developer_instructions"`
	NicknameCandidates    []string `toml:"nickname_candidates"`
	SandboxMode           string   `toml:"sandbox_mode"`
}

func appendCodexAgent(out []AgentFile, file string, ds *diagnostics) []AgentFile {
	raw, err := os.ReadFile(file)
	if err != nil {
		ds.errorf("agent", file, RuleCodexAgentParse, "read failed: %v", err)
		return out
	}
	var w codexAgentWire
	var fields map[string]any
	if err := toml.Unmarshal(raw, &fields); err != nil {
		ds.errorf("agent", file, RuleCodexAgentParse, "invalid TOML: %v", err)
		return out
	}
	if err := toml.Unmarshal(raw, &w); err != nil {
		ds.errorf("agent", file, RuleCodexAgentParse, "invalid field type: %v", err)
		return out
	}
	a := AgentFile{Format: AgentFormatCodex, File: file, Fields: fields, Name: strings.TrimSpace(w.Name),
		Description: strings.TrimSpace(w.Description), Body: w.DeveloperInstructions, Nicknames: w.NicknameCandidates}
	if a.Name == "" || a.Description == "" || strings.TrimSpace(a.Body) == "" || !validSkillName(a.Name) {
		ds.errorf("agent", file, RuleCodexAgentParse, "name, description and developer_instructions are required (name must be a valid identifier)")
		return out
	}
	switch w.SandboxMode {
	case "", "workspace-write", "danger-full-access":
		// 继承父 Agent 沙箱：Polaris 沙箱等级由宿主策略决定，子 Agent 只能收窄不能放宽。
	case "read-only":
		a.ReadOnly = true
	default:
		ds.errorf("agent", file, RuleCodexAgentParse, "unknown sandbox_mode %q", w.SandboxMode)
		return out
	}
	for k := range fields {
		switch k {
		case "name", "description", "developer_instructions", "nickname_candidates", "sandbox_mode":
		case "model", "model_reasoning_effort":
			a.notApplied(ds, k, reasonProviderRouting)
		default:
			a.notApplied(ds, k, "Codex config override without a Polaris subagent equivalent")
		}
	}
	return append(out, a)
}

func (a *AgentFile) notApplied(ds *diagnostics, field, reason string) {
	a.NotApplied = append(a.NotApplied, field)
	sort.Strings(a.NotApplied)
	ds.warnf("agent", a.File, RuleAgentNotApplied, "%s: field %q not applied: %s", a.Name, field, reason)
}

// applyClaudeAgentFields 读取除 name/description 外的 Claude 字段。
func (a *AgentFile) applyClaudeAgentFields(inPlugin bool, ds *diagnostics) {
	a.Model, _ = fmString(a.Fields, "model")
	a.Tools = fmList(a.Fields, "tools", " ,")
	a.DisallowedTools = fmList(a.Fields, "disallowedTools", " ,")
	a.Skills = fmList(a.Fields, "skills", " ,")
	if n, ok := a.Fields["maxTurns"].(int); ok && n > 0 {
		a.MaxTurns = n
	}
	// permissionMode=plan 是只读探索（Claude）；其余模式在 Polaris 由 HITL/PolicyGate 决定，子 Agent 不得放宽。
	if mode, _ := fmString(a.Fields, "permissionMode"); mode == "plan" && !inPlugin {
		a.ReadOnly = true
	}
	notApplied := claudeAgentNotApplied(inPlugin)
	for field := range a.Fields {
		reason, ok := notApplied[field]
		if !ok || (field == "model" && (a.Model == "" || a.Model == "inherit")) {
			continue
		}
		a.notApplied(ds, field, reason)
	}
}
