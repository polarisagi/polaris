package sysadmin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/httputil"

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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	res, err := h.ToolExec(r.Context(), name, body)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	h.ClearToolSchemaCache()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": "skill installed"}) //nolint:errcheck
}

// BuildToolSchemas 收集全部可用工具 schema，用于注入 InferRequest.Tools。
// 2026-07-14（ADR-0051 关联接线）：用 mcp.IsValidLLMName 防御性过滤名称非法
// （不满足 ^[a-zA-Z0-9_-]+$）的条目——MCP server/skill 名称部分来自用户/第三方
// 配置，不受本仓库命名约束，若原样传给要求 function name 满足该正则的
// Provider（如 OpenAI function calling）会导致整次请求被拒绝。此前
// mcp.IsValidLLMName 虽已导出并注明"供 sysadmin.BuildToolSchemas 等外部包
// 防御性过滤使用"，但从未被实际调用过。
func (h *SysAdminHandler) BuildToolSchemas() []types.ToolSchema {
	if h.Catalog == nil {
		return nil
	}
	schemas := h.Catalog.Schemas(context.Background(), types.TrustUntrusted)
	filtered := make([]types.ToolSchema, 0, len(schemas))
	for _, s := range schemas {
		if !mcp.IsValidLLMName(s.Name) {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func (h *SysAdminHandler) ClearToolSchemaCache() {
	if h.Catalog != nil {
		h.Catalog.Invalidate()
	}
}
