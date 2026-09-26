package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
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

// expandUserSkill 输入为 "/name args" 或 "$name args" 且命中 user-invocable 技能时返回渲染内容。
func (r *SlashCommandRouter) expandUserSkill(ctx context.Context, input string) (string, bool) {
	if r.skillReg == nil || r.skillExec == nil {
		return "", false
	}
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "$") {
		return "", false
	}
	name, args, _ := strings.Cut(trimmed[1:], " ")
	sk := r.findUserSkill(ctx, name)
	if sk == nil {
		return "", false
	}
	payload, _ := json.Marshal(map[string]string{"arguments": strings.TrimSpace(args)})
	out, err := r.skillExec.ExecuteSkill(ctx, sk.Name, payload)
	if err != nil {
		slog.Warn("slash: render user skill failed", "skill", sk.Name, "err", err)
		return "", false
	}
	return "<command-name>/" + skillCommandName(*sk) + "</command-name>\n" + string(out), true
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
