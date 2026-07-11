package prompt

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// toolUseEnforcementGuidanceFallback 极简兜底，不替代 embedded 文件。
const toolUseEnforcementGuidanceFallback = "有工具可用时必须立即调用，禁止仅输出执行计划或说明性描述。"

// getToolUseEnforcementModels 返回需要注入工具调用强制引导的模型名称子串（小写匹配）。
func getToolUseEnforcementModels() []string {
	return []string{
		"deepseek", "qwen", "glm", "gpt", "codex", "grok", "gemini", "gemma",
	}
}

// Manager 管理提示词和用户身份（消除包级全局变量）。
// 三层优先级：用户自定义文件 > embedded 内置默认 > 硬编码 fallback。
// 写路径：WriteUserPrompt / DeleteUserPrompt → ~/.polarisagi/polaris/config/prompts/
// 读路径：ReadPrompt → 按三层优先级加载
type Manager struct {
	configDir         string
	embeddedPromptsFS fs.FS
}

var _ protocol.PromptFacade = (*Manager)(nil)

// NewManager 构造 Manager。
func NewManager(configDir string, embeddedPromptsFS fs.FS) *Manager {
	if configDir == "" {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".polarisagi/polaris", "config")
	}
	return &Manager{
		configDir:         configDir,
		embeddedPromptsFS: embeddedPromptsFS,
	}
}

func (pm *Manager) resolveConfigDir() string {
	return pm.configDir
}

// ReadPrompt 按三所有权层优先级读取提示词文件内容。
func (pm *Manager) ReadPrompt(name, fallback string) string {
	if content := pm.loadUserPromptFile(name); content != "" {
		return content
	}
	if content := pm.loadEmbeddedPrompt("prompts/" + name); content != "" {
		return content
	}
	if fallback != "" {
		return fallback
	}
	return protocol.DefaultPolarisIdentityFallback
}

// DefaultIdentity 返回当前生效的 Agent 身份文本（三层优先级）。
func (pm *Manager) DefaultIdentity() string {
	return pm.ReadPrompt("identity.md", protocol.DefaultPolarisIdentityFallback)
}

// NeedsToolUseEnforcement 判断指定模型是否需要注入工具调用强制引导。
func NeedsToolUseEnforcement(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, pattern := range getToolUseEnforcementModels() {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// ModelSpecificGuidance 返回 modelID 对应的模型专属引导文本。
func (pm *Manager) ModelSpecificGuidance(modelID string) string {
	lower := strings.ToLower(modelID)
	switch {
	case containsAny(lower, "deepseek", "qwen", "glm"):
		return pm.ReadPrompt("tool_enforcement/deepseek.md", toolUseEnforcementGuidanceFallback)
	case containsAny(lower, "gpt", "codex", "grok"):
		return pm.ReadPrompt("tool_enforcement/openai.md", toolUseEnforcementGuidanceFallback)
	case containsAny(lower, "gemini", "gemma"):
		return pm.ReadPrompt("tool_enforcement/google.md", toolUseEnforcementGuidanceFallback)
	}
	return ""
}

// GetSoulMD 加载用户自定义身份文件。
func (pm *Manager) GetSoulMD() string {
	if content := pm.loadUserPromptFile("identity.md"); content != "" {
		return content
	}
	b, err := os.ReadFile(filepath.Join(pm.resolveConfigDir(), "SOUL.md"))
	if err == nil {
		return strings.TrimSpace(string(b))
	}
	if content := pm.loadEmbeddedPrompt("prompts/identity.md"); content != "" {
		return content
	}
	return ""
}

// WriteUserPrompt 将用户编辑的提示词写入 config/prompts/{name}。
func (pm *Manager) WriteUserPrompt(name, content string) error {
	dir := filepath.Join(pm.resolveConfigDir(), "prompts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WriteUserPrompt", err)
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
}

// DeleteUserPrompt 删除用户自定义提示词文件，恢复到 embedded 默认。
func (pm *Manager) DeleteUserPrompt(name string) error {
	path := filepath.Join(pm.resolveConfigDir(), "prompts", name)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DeleteUserPrompt", err)
	}
	return nil
}

// ReadPromptDefault 只读取 embedded Layer 0 的默认值，忽略用户文件。
func (pm *Manager) ReadPromptDefault(name string) string {
	if content := pm.loadEmbeddedPrompt("prompts/" + name); content != "" {
		return content
	}
	return protocol.DefaultPolarisIdentityFallback
}

// PlatformHintFor 返回指定平台的提示文本（不区分大小写）。
// 从 configs/prompts/platform/{platform}.md 加载（embedded，只读）。
func (pm *Manager) PlatformHintFor(platform string) string {
	key := strings.ToLower(strings.TrimSpace(platform))
	if key == "" {
		return ""
	}
	return pm.ReadPrompt("platform/"+key+".md", "")
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (pm *Manager) loadUserPromptFile(name string) string {
	b, err := os.ReadFile(filepath.Join(pm.resolveConfigDir(), "prompts", name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (pm *Manager) loadEmbeddedPrompt(name string) string {
	if pm.embeddedPromptsFS == nil {
		return ""
	}
	b, err := fs.ReadFile(pm.embeddedPromptsFS, name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Optimize 异步优化指定 task_type 的 system prompt（Eval Harness 反馈驱动）。
func (pm *Manager) Optimize(ctx context.Context, taskType string) error {
	return apperr.New(apperr.CodeInternal, "prompt: Optimize not yet implemented in Manager")
}
