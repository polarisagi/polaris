package plugin

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/types"
)

type skillInjectionView struct {
	Skill    string   `json:"skill"`
	Display  string   `json:"display_name"`
	PluginID string   `json:"plugin_id,omitempty"`
	Commands []string `json:"commands"`
	Digest   string   `json:"digest"`
	Trusted  bool     `json:"trusted"`
}

// HandleListSkillInjections 列出含动态注入命令的技能及审阅状态（命令原文即审阅对象）。
// GET /v1/skills/injections
func (h *PluginHandler) HandleListSkillInjections(w http.ResponseWriter, r *http.Request) {
	if h.SkillReg == nil {
		http.Error(w, "skill registry not initialized", http.StatusServiceUnavailable)
		return
	}
	skills, err := h.SkillReg.List(r.Context(), types.SkillFilter{})
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	trust, err := h.ExtRepo.ListHookTrust(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	out := []skillInjectionView{}
	for _, sk := range skills {
		points := pluginspec.FindInjections(sk.Instructions)
		if len(points) == 0 {
			continue
		}
		v := skillInjectionView{Skill: sk.Name, Display: sk.DisplayName, PluginID: sk.PluginID, Digest: pluginspec.InjectionDigest(points)}
		for _, p := range points {
			v.Commands = append(v.Commands, p.Command)
		}
		v.Trusted = trust[skill.InjectionTrustKey(sk.Name)] == v.Digest
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Skill < out[j].Skill })
	httputil.WriteJSON(w, map[string]any{"skills": out})
}

// HandleTrustSkillInjection 按命令集合哈希信任技能的动态注入；digest 必须与当前定义一致。
// POST /v1/skills/injections/trust  body: {"skill": "skill:...", "digest": "..."}
func (h *PluginHandler) HandleTrustSkillInjection(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeHookAdmin(w, r) {
		return
	}
	var body struct {
		Skill  string `json:"skill"`
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Skill == "" || body.Digest == "" {
		http.Error(w, "skill and digest required", http.StatusBadRequest)
		return
	}
	meta, err := h.SkillReg.Get(r.Context(), body.Skill, "")
	if err != nil || meta == nil {
		http.Error(w, "skill not found", http.StatusNotFound)
		return
	}
	if pluginspec.InjectionDigest(pluginspec.FindInjections(meta.Instructions)) != body.Digest {
		http.Error(w, "skill commands changed since review; reload and review again", http.StatusConflict)
		return
	}
	if err := h.ExtRepo.SaveHookTrust(r.Context(), skill.InjectionTrustKey(body.Skill), body.Digest); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	httputil.WriteJSON(w, map[string]any{"status": "trusted", "skill": body.Skill})
}
