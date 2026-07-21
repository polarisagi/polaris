package sysadmin

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
)

// HandleCreateSkill 用户意图驱动的技能生成入口（2026-07-21 deadcode 审查补齐，
// ADR-0052）：与 M6 LogicCollapse 的"任务轨迹驱动"生成管线平行，这里是"用户
// 显式描述一个工作流 → LLM 生成 SKILL.md → 安装到技能库"的另一条路径。
// skill.SkillCreator 本身早已实现完整（生成/落盘/安装/注册），此前只是缺一个
// 触发入口——本次选择 HTTP + CLI（polaris skill create）两端配套。
//
// POST /v1/skills/create   body: {"intent": "用户对这个工作流的自然语言描述"}
func (h *SysAdminHandler) HandleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Intent string `json:"intent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	if req.Intent == "" {
		http.Error(w, `{"error":"intent is required"}`, http.StatusBadRequest)
		return
	}
	if h.InstallMgr == nil {
		http.Error(w, `{"error":"extension installer not available"}`, http.StatusServiceUnavailable)
		return
	}

	// Provider 取用方式与 doctor.go/system_prompt.go 等既有生产代码完全一致：
	// PickProvider("default") 为 nil 时兜底 PickProvider("general")。
	p := h.pickSkillCreatorProvider()

	baseDir := filepath.Join(h.DataDir, "skills", "user_generated")
	creator := skill.NewSkillCreator(&skill.ProviderLLMClient{Provider: p}, baseDir, h.InstallMgr, h.SkillReg)

	pluginDir, err := creator.GenerateSkill(r.Context(), req.Intent)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	h.ClearToolSchemaCache()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "plugin_dir": pluginDir})
}

// pickSkillCreatorProvider 复用既有 default→general 兜底链（同 doctor.go /
// chat/system_prompt.go），不为 SkillCreator 单独引入新的 Provider 解析路径。
func (h *SysAdminHandler) pickSkillCreatorProvider() protocol.Provider {
	if h.Registry == nil {
		return nil
	}
	if p := h.Registry.PickProvider("default"); p != nil {
		return p
	}
	return h.Registry.PickProvider("general")
}
