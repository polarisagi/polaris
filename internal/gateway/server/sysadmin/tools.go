package sysadmin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/ffi"
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
	h.ToolSchemaMu.Lock()
	h.ToolSchemaCache = nil
	h.toolEmbedCache = nil // 与 schema 缓存同步清空，确保新工具立即被重新向量化
	h.ToolSchemaMu.Unlock()
}

const (
	// toolSelectThreshold 工具数量超过此阈值时启用语义过滤；低于此值全量注入不会明显浪费 token。
	toolSelectThreshold = 40
	// toolSelectTopK 语义过滤后注入 LLM 的最大工具数量。
	toolSelectTopK = 20
	// toolEmbedCacheMax 工具描述向量缓存条目上限；超出后随 schema 缓存一同清空。
	toolEmbedCacheMax = 1024
)

// SelectToolSchemas 按用户 query 语义相似度选取最相关的 tool schema 子集注入 LLM。
//
// 降级条件（满足任一则全量返回）：
//   - Embedder 未注入（nil）
//   - 工具总数 ≤ toolSelectThreshold
//   - query 为空或向量化失败（Ollama 未启动等）
//
// 正常路径：余弦相似度排序取 top-K，并记录 debug 日志。
func (h *SysAdminHandler) SelectToolSchemas(query string) []types.ToolSchema { //nolint:cyclop
	all := h.BuildToolSchemas()
	if h.Embedder == nil || len(all) <= toolSelectThreshold || query == "" {
		return all
	}

	// 向量化 query（在任何锁外执行，避免 Ollama HTTP 调用持锁阻塞）
	queryVec := h.Embedder.Embed(query)
	if len(queryVec) == 0 {
		// Embedder 暂不可用（Ollama 未启动 / 冷启动），全量降级
		return all
	}

	// 为每个 schema 取缓存向量，计算余弦相似度
	type scored struct {
		schema types.ToolSchema
		score  float32
	}
	candidates := make([]scored, 0, len(all))
	for _, s := range all {
		key := toolEmbedKey(s.Name, s.Description)
		vec := h.getOrEmbedTool(key, s.Name+" "+s.Description)
		if len(vec) == 0 {
			// 向量化失败（罕见）：保留工具，赋予中等分数，防止高频工具被完全丢弃
			candidates = append(candidates, scored{s, 0.5})
			continue
		}
		candidates = append(candidates, scored{s, ffi.VecCosineF32(queryVec, vec)})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	topK := toolSelectTopK
	if topK > len(candidates) {
		topK = len(candidates)
	}
	result := make([]types.ToolSchema, topK)
	for i := range result {
		result[i] = candidates[i].schema
	}
	slog.Debug("sysadmin: SelectToolSchemas filtered",
		"total", len(all), "selected", topK, "query_prefix", truncateQueryStr(query, 40))
	return result
}

// getOrEmbedTool 返回工具描述向量（读缓存优先；缓存 miss 时调用 Embedder 并回填）。
// 双检锁：Embedder.Embed 在锁外执行，避免 Ollama HTTP 调用持锁阻塞并发请求。
func (h *SysAdminHandler) getOrEmbedTool(key, text string) []float32 {
	h.ToolSchemaMu.RLock()
	if h.toolEmbedCache != nil {
		if v, ok := h.toolEmbedCache[key]; ok {
			h.ToolSchemaMu.RUnlock()
			return v
		}
	}
	h.ToolSchemaMu.RUnlock()

	// 锁外向量化（可能较慢）
	v := h.Embedder.Embed(text)
	if len(v) == 0 {
		return nil
	}

	h.ToolSchemaMu.Lock()
	if h.toolEmbedCache == nil {
		h.toolEmbedCache = make(map[string][]float32, toolEmbedCacheMax)
	}
	if len(h.toolEmbedCache) < toolEmbedCacheMax {
		h.toolEmbedCache[key] = v
	}
	h.ToolSchemaMu.Unlock()
	return v
}

// toolEmbedKey 生成工具向量缓存键（sha256(name+"\x00"+desc) 的二进制字符串，直接用作 map key）。
func toolEmbedKey(name, desc string) string {
	h := sha256.Sum256([]byte(name + "\x00" + desc))
	return string(h[:])
}

// truncateQueryStr 截取字符串前 n 个字节（用于日志，避免过长）。
func truncateQueryStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
