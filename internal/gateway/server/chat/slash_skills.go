package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SetSkills 注入技能注册表与执行器，使用户可按 /技能名、/插件:技能 或 Codex 的 $技能名 调用
// user-invocable 技能（ADR-0103 决策五）。未注入时仅内置命令可用。
func (r *SlashCommandRouter) SetSkills(reg protocol.SkillRegistry, exec protocol.SkillExecutor) {
	r.skillReg = reg
	r.skillExec = exec
}

// UserSkillCommand 用户可调用技能的命令视图（供前端补全）。
type UserSkillCommand struct {
	Command      string `json:"command"` // "/plugin:skill"
	Description  string `json:"description"`
	ArgumentHint string `json:"argument_hint,omitempty"`
	Kind         string `json:"kind"`
}

// ListUserSkillCommands 列出用户可调用技能（排序稳定）。
func (r *SlashCommandRouter) ListUserSkillCommands(ctx context.Context) []UserSkillCommand {
	if r.skillReg == nil {
		return nil
	}
	skills, err := r.skillReg.List(ctx, types.SkillFilter{})
	if err != nil {
		slog.Warn("slash: list skills failed", "err", err)
		return nil
	}
	var out []UserSkillCommand
	for _, sk := range skills {
		if sk.DisableUserInvocation || sk.Deprecated {
			continue
		}
		var spec struct {
			ArgumentHint string `json:"argument_hint"`
		}
		if sk.Spec != "" {
			if err := json.Unmarshal([]byte(sk.Spec), &spec); err != nil {
				// L3：spec 损坏只影响补全提示，命令本身仍可调用；留痕。
				slog.Warn("slash: corrupt skill spec", "skill", sk.Name, "err", err)
			}
		}
		out = append(out, UserSkillCommand{Command: "/" + skillCommandName(sk), Description: sk.Description,
			ArgumentHint: spec.ArgumentHint, Kind: sk.Kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	return out
}

// skillInvocation 用户调用技能的渲染结果；forkAgent 非空表示技能声明 context: fork。
type skillInvocation struct {
	content   string
	forkAgent string
}

// expandUserSkill 输入为 "/name args" 或 "$name args" 且命中 user-invocable 技能时返回渲染内容。
func (r *SlashCommandRouter) expandUserSkill(ctx context.Context, input string) (skillInvocation, bool) {
	if r.skillReg == nil || r.skillExec == nil {
		return skillInvocation{}, false
	}
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "$") {
		return skillInvocation{}, false
	}
	name, args, _ := strings.Cut(trimmed[1:], " ")
	sk := r.findUserSkill(ctx, name)
	if sk == nil {
		return skillInvocation{}, false
	}
	payload, _ := json.Marshal(map[string]string{"arguments": strings.TrimSpace(args)})
	out, err := r.skillExec.ExecuteSkill(ctx, sk.Name, payload)
	if err != nil {
		slog.Warn("slash: render user skill failed", "skill", sk.Name, "err", err)
		return skillInvocation{}, false
	}
	return skillInvocation{content: "<command-name>/" + skillCommandName(*sk) + "</command-name>\n" + string(out),
		forkAgent: forkAgentOf(*sk)}, true
}

// forkAgentOf context: fork 时返回目标子 Agent（缺省 general-purpose，Claude 规则）。
func forkAgentOf(sk types.SkillMeta) string {
	var spec struct {
		Context string `json:"context"`
		Agent   string `json:"agent"`
	}
	if sk.Spec == "" || json.Unmarshal([]byte(sk.Spec), &spec) != nil || spec.Context != "fork" {
		return ""
	}
	if spec.Agent == "" {
		return "general-purpose"
	}
	return spec.Agent
}

// SubagentRunner 子 Agent 执行（orchestrator.SubagentRunner 实现）。
type SubagentRunner interface {
	RunSubagent(ctx context.Context, parentSessionID, agent, prompt string) (string, error)
}

// SetSubagents 注入子 Agent 执行器，启用 context: fork 技能的用户调用。
func (r *SlashCommandRouter) SetSubagents(s SubagentRunner) { r.subagents = s }

// runForkedSkill fork 技能：渲染内容作为子 Agent 的任务，无会话历史（Claude 语义）；子 Agent
// 最终输出作为本轮回复，由会话层持久化。
func (r *SlashCommandRouter) runForkedSkill(ctx context.Context, sessionID string, inv skillInvocation,
	history []types.Message, sink session.Sink,
) session.CommandResult {
	_ = sink.Emit(session.Event{Kind: session.KindStatus, Payload: map[string]any{"type": "subagent", "agent": inv.forkAgent,
		"message": "running in subagent " + inv.forkAgent}})
	out, err := r.subagents.RunSubagent(ctx, sessionID, inv.forkAgent, inv.content)
	if err != nil {
		slog.Warn("slash: forked skill failed", "agent", inv.forkAgent, "err", err)
		out = "Subagent " + inv.forkAgent + " failed: " + err.Error()
	}
	_ = sink.Emit(session.Event{Kind: session.KindDelta, Text: out})
	return session.CommandResult{Handled: true, Response: out, UpdatedHistory: history}
}

// findUserSkill 按对外名匹配（"plugin:skill"、裸技能名，大小写不敏感）；
// 裸名同时命中多个插件技能时视为歧义不匹配，用户须使用带插件前缀的全名（Claude 规则）。
func (r *SlashCommandRouter) findUserSkill(ctx context.Context, name string) *types.SkillMeta {
	skills, err := r.skillReg.List(ctx, types.SkillFilter{})
	if err != nil || name == "" {
		return nil
	}
	name = strings.ToLower(name)
	var bare []types.SkillMeta
	for i := range skills {
		sk := skills[i]
		if sk.DisableUserInvocation {
			continue
		}
		full := strings.ToLower(skillCommandName(sk))
		if full == name {
			return &skills[i]
		}
		if i := strings.LastIndex(full, ":"); i >= 0 && full[i+1:] == name {
			bare = append(bare, sk)
		}
	}
	if len(bare) == 1 {
		return &bare[0]
	}
	return nil
}

func skillCommandName(sk types.SkillMeta) string {
	if sk.DisplayName != "" {
		return sk.DisplayName
	}
	return strings.TrimPrefix(sk.Name, types.SkillPrefix)
}

// HandleListSkillCommands 返回用户可调用技能命令（前端 "/" 与 "$" 补全）。
// GET /v1/skills/commands
func (h *ChatHandler) HandleListSkillCommands(w http.ResponseWriter, r *http.Request) {
	var cmds []UserSkillCommand
	if h.SlashRouter != nil {
		cmds = h.SlashRouter.ListUserSkillCommands(r.Context())
	}
	if cmds == nil {
		cmds = []UserSkillCommand{}
	}
	httputil.WriteJSON(w, map[string]any{"commands": cmds})
}
