package sysadmin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// ToolInfo 工具列表 API 响应条目。
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"` // "builtin" | "mcp"
	RiskLevel   int    `json:"risk_level,omitempty"`
	Connected   bool   `json:"connected,omitempty"` // MCP 工具专用
}

// SkillInfo skill 列表 API 响应条目。
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Enabled     bool   `json:"enabled"`
	ExecMode    string `json:"exec_mode"`           // "tool" | "ambient"
	Source      string `json:"source"`              // "builtin" | "script"
	PluginID    string `json:"plugin_id,omitempty"` // 来自插件时填充
}

// handleListTools 返回所有已注册工具（builtin + MCP）。
// GET /v1/tools
func (h *SysAdminHandler) HandleListTools(w http.ResponseWriter, r *http.Request) {
	var tools []ToolInfo

	if h.Catalog != nil {
		entries := h.Catalog.List(context.Background(), types.TrustUntrusted)
		for _, e := range entries {
			if e.Source == types.ToolSkill {
				continue // 技能已经在单独的面板（/v1/skills）展示，避免在工具列表重复
			}
			tools = append(tools, ToolInfo{
				Name:        e.Name,
				Description: e.Description,
				Source:      string(e.Source),
				Connected:   true,
			})
		}
	}

	if tools == nil {
		tools = []ToolInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tools": tools}) //nolint:errcheck
}

// handleListSkills 返回所有已注册 skill。
// GET /v1/skills
func (h *SysAdminHandler) HandleListSkills(w http.ResponseWriter, r *http.Request) {
	var skills []SkillInfo

	if h.SkillReg != nil {
		metas, err := h.SkillReg.List(r.Context(), types.SkillFilter{IncludeDeprecated: true})
		if err == nil {
			for _, m := range metas {
				// 从 capabilities 数组中提取 description（格式：["description:xxx", "capability:yyy"]）
				desc := ""
				for _, cap := range m.Capabilities {
					if d, ok := strings.CutPrefix(cap, "description:"); ok {
						desc = d
						break
					}
				}
				skills = append(skills, SkillInfo{
					Name:        m.Name,
					Description: desc,
					Version:     m.Version,
					Enabled:     !m.Deprecated,
					ExecMode:    m.ExecMode,
					Source:      m.Runtime,
					PluginID:    m.PluginID,
				})
			}
		}
	}

	if skills == nil {
		skills = []SkillInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"skills": skills, "total": len(skills)}) //nolint:errcheck
}

// handleListToolSchemas 返回可注入 LLM 的 tool schema 列表（供调试用）。
// 复用 buildToolSchemas，暴露给前端检查工具注入是否正确。
func (h *SysAdminHandler) HandleListToolSchemas(w http.ResponseWriter, _ *http.Request) {
	schemas := h.BuildToolSchemas()
	if schemas == nil {
		schemas = []types.ToolSchema{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"schemas": schemas, "total": len(schemas)}) //nolint:errcheck
}

// handleExecuteTool 直接执行工具（内置或 MCP）。
// POST /v1/tools/{name}/execute
func (h *SysAdminHandler) HandleExecuteTool(w http.ResponseWriter, r *http.Request) {
	if h.ToolExec == nil {
		http.Error(w, "tool executor not available", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing tool name", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	res, err := h.ToolExec(r.Context(), name, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res) //nolint:errcheck
}

// handleInstallSkill 接受 Wasm 载荷或源码，注册到技能库中。
// POST /v1/skills/install
func (h *SysAdminHandler) HandleInstallSkill(w http.ResponseWriter, r *http.Request) {
	if h.SkillReg == nil {
		http.Error(w, "skill registry not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
		Runtime     string `json:"runtime"`     // "script" | "builtin"
		ScriptPath  string `json:"script_path"` // 技能脚本安装路径
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	meta := types.SkillMeta{
		Name:       req.Name,
		Version:    req.Version,
		Runtime:    req.Runtime,
		ScriptPath: req.ScriptPath,
		Deprecated: false,
	}

	if err := h.SkillReg.Register(r.Context(), meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.ClearToolSchemaCache()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": "skill installed"}) //nolint:errcheck
}

func (h *SysAdminHandler) BuildToolSchemas() []types.ToolSchema {
	if h.Catalog != nil {
		return h.Catalog.Schemas(context.Background(), types.TrustUntrusted)
	}
	return nil
}

func (h *SysAdminHandler) ClearToolSchemaCache() {
	if h.Catalog != nil {
		h.Catalog.Invalidate()
	}
}
