package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AgentDefinition 可委派子 Agent 的对外视图（列表 API / list_agents）。
type AgentDefinition struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"` // plugin:<id> / project / user / builtin
	Format      string   `json:"format"` // claude / codex / builtin
	File        string   `json:"file"`
	Tools       []string `json:"tools,omitempty"`
	Disallowed  []string `json:"disallowed_tools,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	MaxTurns    int      `json:"max_turns,omitempty"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	Model       string   `json:"model,omitempty"`
	Thinking    string   `json:"thinking,omitempty"`
	NotApplied  []string `json:"not_applied,omitempty"`

	pluginName string
	body       string
}

// ExpertDefinition 保留插件来源身份，避免同名角色跨插件合并。
type ExpertDefinition struct {
	PluginID   string `json:"plugin_id"`
	PluginName string `json:"plugin_name"`
	pluginspec.Expert
	Agent AgentDefinition `json:"agent"`
}

// SkillContentLoader 读取技能渲染后的正文（Claude agents `skills` 字段：启动时注入完整技能内容）。
// 由技能执行器提供，动态注入审阅、参数渲染规则与模型调用一致。
type SkillContentLoader func(ctx context.Context, skillName string) (string, error)

// AgentDefinitionProvider 子 Agent 定义来源（ADR-0103 决策三）：项目 <root>/.polaris/agents、
// 用户 <data>/agents（Claude .md 与 Codex .toml）、两家内置类型与已启用插件的 agents/（命名
// "<插件>:<agent>"）。同名优先级：项目 > 用户 > 内置；插件 agent 自带命名空间不与之冲突。
type AgentDefinitionProvider struct {
	extRepo     protocol.ExtensionRepository
	dataDir     string
	projectDirs func() []string
	loadSkill   SkillContentLoader
}

func NewAgentDefinitionProvider(extRepo protocol.ExtensionRepository, dataDir string, projectDirs func() []string, loadSkill SkillContentLoader) *AgentDefinitionProvider {
	return &AgentDefinitionProvider{extRepo: extRepo, dataDir: dataDir, projectDirs: projectDirs, loadSkill: loadSkill}
}

// UserAgentsDir 用户级 agents 目录。
func UserAgentsDir(dataDir string) string { return filepath.Join(dataDir, "agents") }

// ListAgentDefinitions 返回全部可委派定义与解析诊断；单个文件出错不影响其余定义。
func (p *AgentDefinitionProvider) ListAgentDefinitions(ctx context.Context) ([]AgentDefinition, []pluginspec.Diagnostic, error) {
	seen := map[string]bool{}
	var out []AgentDefinition
	var diags []pluginspec.Diagnostic
	addDir := func(dir, source string) {
		agents, ds := pluginspec.ListAgentDir(dir)
		diags = append(diags, ds...)
		for _, a := range agents {
			if seen[a.Name] {
				continue
			}
			seen[a.Name] = true
			out = append(out, definitionFrom(a, a.Name, source, ""))
		}
	}
	if p.projectDirs != nil {
		for _, root := range p.projectDirs() {
			addDir(filepath.Join(root, ".polaris", "agents"), "project")
		}
	}
	addDir(UserAgentsDir(p.dataDir), "user")
	for _, b := range builtinAgents() {
		if !seen[b.Name] {
			seen[b.Name] = true
			b.Source, b.Format = "builtin", "builtin"
			out = append(out, b)
		}
	}
	plugins, pluginDiags, err := p.pluginAgents(ctx)
	if err != nil {
		return nil, nil, err
	}
	return append(out, plugins...), append(diags, pluginDiags...), nil
}

// pluginAgents 插件 agent 正文不入库（清单 JSON 不含 Body），按安装目录内文件重新解析。
func (p *AgentDefinitionProvider) pluginAgents(ctx context.Context) ([]AgentDefinition, []pluginspec.Diagnostic, error) {
	rows, err := p.extRepo.ListPlugins(ctx)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeOf(err), "AgentDefinitionProvider.plugins", err)
	}
	var out []AgentDefinition
	var diags []pluginspec.Diagnostic
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		var spec pluginspec.Plugin
		if err := json.Unmarshal([]byte(row.Manifest), &spec); err != nil {
			slog.Warn("agent: corrupt plugin manifest, agents skipped", "plugin", row.ID, "err", err)
			continue
		}
		for _, entry := range spec.Agents {
			a, ds, ok := pluginspec.ParseAgentFile(entry.File, entry.Name, true)
			diags = append(diags, ds...)
			if !ok || !withinRoot(row.InstallPath, entry.File) {
				continue
			}
			out = append(out, definitionFrom(a, row.Name+":"+entry.Name, "plugin:"+row.ID, row.Name))
		}
	}
	return out, diags, nil
}

// ListExperts 以清单声明作为发现边界，避免任意子 Agent 被自动公开。
func (p *AgentDefinitionProvider) ListExperts(ctx context.Context) ([]ExpertDefinition, []pluginspec.Diagnostic, error) {
	rows, err := p.extRepo.ListPlugins(ctx)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeOf(err), "AgentDefinitionProvider.experts", err)
	}
	var out []ExpertDefinition
	var diags []pluginspec.Diagnostic
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		var spec pluginspec.Plugin
		if err := json.Unmarshal([]byte(row.Manifest), &spec); err != nil {
			continue
		}
		for _, expert := range spec.Experts {
			canonical := row.Name + ":" + expert.EntryAgent
			var def *AgentDefinition
			for _, entry := range spec.Agents {
				if entry.Name != expert.EntryAgent || !withinRoot(row.InstallPath, entry.File) {
					continue
				}
				a, ds, ok := pluginspec.ParseAgentFile(entry.File, entry.Name, true)
				diags = append(diags, ds...)
				if !ok {
					continue
				}
				x := definitionFrom(a, canonical, "plugin:"+row.ID, row.Name)
				def = &x
				break
			}
			if def == nil {
				continue
			}
			out = append(out, ExpertDefinition{PluginID: row.ID, PluginName: row.Name, Expert: expert, Agent: *def})
		}
	}
	return out, diags, nil
}

// ResolveNamedExpert 限定插件来源，避免同后缀角色跨插件回退或误绑定。
func (p *AgentDefinitionProvider) ResolveNamedExpert(ctx context.Context, name string) (*types.AgentProfileSpec, *ExpertDefinition, error) {
	experts, _, err := p.ListExperts(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range experts {
		if experts[i].Agent.Name != name {
			continue
		}
		instructions, err := p.withPreloadedSkills(ctx, experts[i].Agent)
		if err != nil {
			return nil, nil, err
		}
		profile := &types.AgentProfileSpec{Name: experts[i].Agent.Name, Source: experts[i].Agent.Source, Instructions: instructions,
			InstructionTaint: definitionTaint(experts[i].Agent.Source), Tools: experts[i].Agent.Tools,
			DisallowedTools: experts[i].Agent.Disallowed, ReadOnly: experts[i].Agent.ReadOnly,
			MaxTurns: experts[i].Agent.MaxTurns, Model: experts[i].Agent.Model, Thinking: experts[i].Agent.Thinking,
			AllowDelegation: experts[i].Type == "team"}
		if experts[i].Type == "team" {
			for _, member := range experts[i].Members {
				profile.AllowedAgentTargets = append(profile.AllowedAgentTargets, experts[i].PluginName+":"+member)
			}
		}
		return profile, &experts[i], nil
	}
	return nil, nil, apperr.New(apperr.CodeNotFound, fmt.Sprintf("unknown installed expert %q", name))
}

func withinRoot(root, file string) bool {
	rel, err := filepath.Rel(root, file)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func definitionFrom(a pluginspec.AgentFile, name, source, pluginName string) AgentDefinition {
	return AgentDefinition{Name: name, Description: a.Description, Source: source, Format: a.Format, File: a.File,
		Tools: a.Tools, Disallowed: a.DisallowedTools, Skills: a.Skills, MaxTurns: a.MaxTurns, ReadOnly: a.ReadOnly,
		Model: a.Model, Thinking: a.Thinking,
		NotApplied: a.NotApplied, pluginName: pluginName, body: a.Body}
}

// ResolveAgentProfile 名称 → 运行期角色规格；未知名称返回 CodeNotFound。
func (p *AgentDefinitionProvider) ResolveAgentProfile(ctx context.Context, name string) (*types.AgentProfileSpec, error) {
	defs, _, err := p.ListAgentDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range defs {
		if d.Name != name {
			continue
		}
		instructions, err := p.withPreloadedSkills(ctx, d)
		if err != nil {
			return nil, err
		}
		return &types.AgentProfileSpec{Name: d.Name, Source: d.Source, Instructions: instructions,
			InstructionTaint: definitionTaint(d.Source), Tools: d.Tools, DisallowedTools: d.Disallowed,
			ReadOnly: d.ReadOnly, MaxTurns: d.MaxTurns, Model: d.Model, Thinking: d.Thinking}, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, fmt.Sprintf("unknown agent %q", name))
}

// definitionTaint 内置与用户目录由宿主/本机用户编写（TaintLow）；插件与项目目录内容可来自第三方
// 仓库，与 AGENTS.md 同一威胁模型（TaintMedium，写入时叠加 Spotlighting）。
func definitionTaint(source string) types.TaintLevel {
	if source == "user" || source == "builtin" {
		return types.TaintLow
	}
	return types.TaintMedium
}

// withPreloadedSkills Claude：skills 字段列出的技能在子 Agent 启动时完整注入。插件 agent 先按
// 本插件命名空间解析；技能缺失或不可加载时失败——少注入会让角色在缺知识的状态下静默运行。
func (p *AgentDefinitionProvider) withPreloadedSkills(ctx context.Context, d AgentDefinition) (string, error) {
	if len(d.Skills) == 0 {
		return d.body, nil
	}
	if p.loadSkill == nil {
		return "", apperr.New(apperr.CodeInternal, "agent "+d.Name+": skill preloading unavailable")
	}
	var sb strings.Builder
	sb.WriteString(d.body)
	for _, s := range d.Skills {
		content, err := p.loadPreloadSkill(ctx, d, s)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeOf(err), fmt.Sprintf("agent %s: preload skill %s", d.Name, s), err)
		}
		fmt.Fprintf(&sb, "\n\n<skill name=%q>\n%s\n</skill>", s, content)
	}
	return sb.String(), nil
}

func (p *AgentDefinitionProvider) loadPreloadSkill(ctx context.Context, d AgentDefinition, ref string) (string, error) {
	var candidates []string
	if plugin, skill, ok := strings.Cut(ref, ":"); ok {
		candidates = append(candidates, PluginSkillName(plugin, skill))
	} else if d.pluginName != "" {
		candidates = append(candidates, PluginSkillName(d.pluginName, ref))
	}
	candidates = append(candidates, StandaloneSkillName(ref))
	var lastErr error
	for _, name := range candidates {
		content, err := p.loadSkill(ctx, name)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !apperr.IsCode(err, apperr.CodeNotFound) {
			break
		}
	}
	return "", lastErr
}
