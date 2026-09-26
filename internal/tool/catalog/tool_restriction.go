package catalog

import (
	"context"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// claudeToolAliases Claude 工具名 → Polaris 内置工具名（ADR-0103 决策三：agents 的 tools /
// disallowedTools 按 Claude 工具名书写）。Polaris 原生名直接书写同样有效。
func claudeToolAliases() map[string][]string {
	return map[string][]string{
		"Read":         {"read_file", "notebook_read", "read_tool_ref"},
		"Write":        {"write_file"},
		"Edit":         {"str_replace_editor"},
		"MultiEdit":    {"multi_edit"},
		"Glob":         {"glob"},
		"Grep":         {"grep"},
		"LS":           {"list_dir"},
		"Bash":         {"bash", "run_command"},
		"WebFetch":     {"fetch_url"},
		"WebSearch":    {"web_search"},
		"NotebookEdit": {"notebook_edit"},
		"NotebookRead": {"notebook_read"},
		"TodoWrite":    {"todo_write", "todo_read"},
		"ToolSearch":   {"tool_search"},
		"Agent":        {DelegateToolName, ListAgentsToolName},
		"Task":         {DelegateToolName, ListAgentsToolName}, // Claude 旧名（v2.1.63 起为 Agent）
	}
}

// DelegateToolName / ListAgentsToolName 委派子 Agent 的内核工具（执行由 Agent 内核特判）。
const (
	DelegateToolName   = "transfer_to_agent"
	ListAgentsToolName = "list_agents"
)

// codeActPrefix code_act:<lang> 由 Bash 授权覆盖（均为在沙箱中执行任意代码）。
const codeActPrefix = "code_act:"

// ToolRestriction 子 Agent 工具白/黑名单（deny 优先）。零值 / nil = 不限制。
type ToolRestriction struct {
	restrictAllow bool
	allow         toolMatcher
	deny          toolMatcher
	// Ignored 无法安全映射而按拒绝处置的条目（如 Bash(git *) 这类参数级规则），供诊断展示。
	Ignored []string
}

type toolMatcher struct {
	names        map[string]bool
	agentTargets map[string]bool // nil = 不限定委派目标
	prefixes     []string
	allSkills    bool
	allCodeAct   bool
}

// NewToolRestriction tools 为空 = 继承全部（Claude 语义）；两者皆空返回 nil。
func NewToolRestriction(tools, disallowed []string) *ToolRestriction {
	if len(tools) == 0 && len(disallowed) == 0 {
		return nil
	}
	r := &ToolRestriction{restrictAllow: len(tools) > 0}
	r.Ignored = r.allow.add(tools, true)
	r.deny.add(disallowed, false)
	return r
}

// add 返回无法映射的条目。白名单中带参数说明符的条目（Bash(git *)）整体忽略——放行整个工具
// 会越过作者声明的范围；黑名单中则按整个工具拒绝——宁可多拒，不可漏拒。
// Agent(a, b) 额外限定可委派目标，Skill(a, b) 限定具体技能（Claude 规则）。
func (m *toolMatcher) add(entries []string, allowList bool) []string {
	if m.names == nil {
		m.names = map[string]bool{}
	}
	var ignored []string
	for _, raw := range entries {
		name, spec, hasSpec := splitSpecifier(strings.TrimSpace(raw))
		switch {
		case name == "":
		case !hasSpec || !allowList:
			m.addName(name)
		case name == "Agent" || name == "Task":
			m.addName(name)
			m.addAgentTargets(spec)
		case name == "Skill":
			for s := range strings.SplitSeq(spec, ",") {
				slug := skillSlug(strings.TrimSpace(s))
				m.names[slug], m.names["skill__"+slug] = true, true
			}
		default:
			ignored = append(ignored, raw)
		}
	}
	return ignored
}

func (m *toolMatcher) addAgentTargets(spec string) {
	if m.agentTargets == nil {
		m.agentTargets = map[string]bool{}
	}
	for t := range strings.SplitSeq(spec, ",") {
		if t = strings.TrimSpace(t); t != "" {
			m.agentTargets[t] = true
		}
	}
}

func (m *toolMatcher) addName(name string) {
	switch {
	case name == "*":
		m.prefixes = append(m.prefixes, "")
	case name == "Skill":
		m.allSkills = true
	case name == "Bash":
		m.allCodeAct = true
		for _, n := range claudeToolAliases()[name] {
			m.names[n] = true
		}
	case strings.HasPrefix(name, "mcp__"):
		m.addMCP(name)
	default:
		if aliases, ok := claudeToolAliases()[name]; ok {
			for _, n := range aliases {
				m.names[n] = true
			}
			return
		}
		m.names[name] = true
	}
}

// addMCP mcp__server__tool 精确；mcp__server 与 mcp__server__* 为整台服务器（Claude 规则）。
func (m *toolMatcher) addMCP(name string) {
	if prefix, ok := strings.CutSuffix(name, "*"); ok {
		m.prefixes = append(m.prefixes, prefix)
		return
	}
	if strings.Count(name, "__") == 1 {
		m.prefixes = append(m.prefixes, name+"__")
		return
	}
	m.names[name] = true
}

func (m *toolMatcher) match(name string, src types.ToolSource) bool {
	if m.names[name] || (m.allSkills && src == types.ToolSkill) || (m.allCodeAct && strings.HasPrefix(name, codeActPrefix)) {
		return true
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Permits 判断工具是否在限制内。src 未知时传空串（仅按名称判断）。
func (r *ToolRestriction) Permits(name string, src types.ToolSource) bool {
	if r == nil {
		return true
	}
	if r.deny.match(name, src) {
		return false
	}
	return !r.restrictAllow || r.allow.match(name, src)
}

// PermitsAgent 判断能否委派给指定子 Agent（白名单 Agent(a, b) 限定时生效）。
func (r *ToolRestriction) PermitsAgent(target string) bool {
	if r == nil {
		return true
	}
	if !r.Permits(DelegateToolName, types.ToolBuiltin) {
		return false
	}
	return r.allow.agentTargets == nil || r.allow.agentTargets[target]
}

func splitSpecifier(s string) (name, spec string, ok bool) {
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return s, "", false
	}
	return strings.TrimSpace(s[:open]), strings.TrimSpace(s[open+1 : len(s)-1]), true
}

// skillSlug 技能名 → 目录中的模型调用名（SkillCatalog：去 "skill:" 前缀；插件命名空间 ":" 换 "__"）。
// 注册表内同一技能以 "skill__" + slug 登记，两者都要匹配。
func skillSlug(name string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, types.SkillPrefix), ":", "__")
}

// Restrict 返回只暴露受限工具的只读目录视图；r 为 nil 时原样返回。
// 视图只影响模型可见性，执行期仍须以 Permits 硬拦截（可见性过滤不是安全边界）。
func Restrict(c Catalog, r *ToolRestriction) Catalog {
	if c == nil || r == nil {
		return c
	}
	return restrictedCatalog{Catalog: c, r: r}
}

type restrictedCatalog struct {
	Catalog
	r *ToolRestriction
}

func (c restrictedCatalog) List(ctx context.Context, minTrust types.TrustTier) []protocol.CatalogEntry {
	all := c.Catalog.List(ctx, minTrust)
	out := make([]protocol.CatalogEntry, 0, len(all))
	for _, e := range all {
		if c.r.Permits(e.Name, e.Source) {
			out = append(out, e)
		}
	}
	return out
}

func (c restrictedCatalog) Lookup(name string) (protocol.CatalogEntry, bool) {
	e, ok := c.Catalog.Lookup(name)
	if !ok || !c.r.Permits(e.Name, e.Source) {
		return protocol.CatalogEntry{}, false
	}
	return e, true
}

func (c restrictedCatalog) Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema {
	all := c.Catalog.Schemas(ctx, minTrust)
	out := make([]types.ToolSchema, 0, len(all))
	for _, s := range all {
		src := types.ToolSource("")
		if e, ok := c.Catalog.Lookup(s.Name); ok {
			src = e.Source
		}
		if c.r.Permits(s.Name, src) {
			out = append(out, s)
		}
	}
	return out
}
