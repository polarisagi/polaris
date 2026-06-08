package server

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/pkg/cognition/memory"
)

// allowedUserPrompts 用户可通过 API 编辑的提示词文件名白名单。
// Layer 0（embedded tool_enforcement / platform）不在此列——那是产品行为逻辑，不暴露给用户。
var allowedUserPrompts = map[string]string{
	"identity":            "identity.md",
	"custom_instructions": "custom_instructions.md",
}

// PromptEntry API 响应中的单条提示词描述。
type PromptEntry struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	CurrentValue string `json:"current_value"`
	DefaultValue string `json:"default_value"`
	IsCustomized bool   `json:"is_customized"`
}

// handleListPrompts GET /v1/config/prompts
// 返回所有用户可编辑的提示词及其当前值与内置默认值。
func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	descriptions := map[string]string{
		"identity":            "Agent 身份文本（我是谁）。覆盖内置默认，整段替换。",
		"custom_instructions": "追加的行为指令（我应该怎么做）。拼接到身份文本之后。",
	}

	entries := make([]PromptEntry, 0, len(allowedUserPrompts))
	for name, filename := range allowedUserPrompts {
		defaultVal := memory.ReadPromptDefault(filename)
		currentVal := memory.ReadPrompt(filename, defaultVal)
		entries = append(entries, PromptEntry{
			Name:         name,
			Description:  descriptions[name],
			CurrentValue: currentVal,
			DefaultValue: defaultVal,
			IsCustomized: currentVal != defaultVal,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// handleGetPrompt GET /v1/config/prompts/{name}
func (s *Server) handleGetPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	filename, ok := allowedUserPrompts[name]
	if !ok {
		http.Error(w, "unknown prompt: "+name, http.StatusNotFound)
		return
	}

	defaultVal := memory.ReadPromptDefault(filename)
	currentVal := memory.ReadPrompt(filename, defaultVal)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PromptEntry{
		Name:         name,
		CurrentValue: currentVal,
		DefaultValue: defaultVal,
		IsCustomized: currentVal != defaultVal,
	})
}

// handleSetPrompt PUT /v1/config/prompts/{name}
// 将用户编辑的提示词写入 ~/.polarisagi/polaris/config/prompts/{filename}。
// 立即热更新 ImmutableCore，下一轮对话生效。
func (s *Server) handleSetPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	filename, ok := allowedUserPrompts[name]
	if !ok {
		http.Error(w, "unknown prompt: "+name, http.StatusNotFound)
		return
	}

	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Value) > 32*1024 {
		http.Error(w, "prompt too large (max 32KB)", http.StatusBadRequest)
		return
	}

	if err := memory.WriteUserPrompt(filename, req.Value); err != nil {
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 热更新 ImmutableCore（本次写入的变更在下一轮 injectSystemPrompt 生效；
	// 此处同步更新 soulMDContent 以支持立即生效）
	if name == "identity" {
		s.soulMDContent = req.Value
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "name": name})
}

// handleResetPrompt DELETE /v1/config/prompts/{name}
// 删除用户自定义提示词文件，恢复到 embedded 内置默认。
func (s *Server) handleResetPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	filename, ok := allowedUserPrompts[name]
	if !ok {
		http.Error(w, "unknown prompt: "+name, http.StatusNotFound)
		return
	}

	if err := memory.DeleteUserPrompt(filename); err != nil {
		http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 热更新：恢复到内置默认
	if name == "identity" {
		s.soulMDContent = memory.LoadSoulMD() // 重新走三层加载（文件删除后回落到 embedded）
	}

	defaultVal := memory.ReadPromptDefault(filename)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":        "ok",
		"name":          name,
		"restored_to":   "default",
		"default_value": defaultVal,
	})
}
