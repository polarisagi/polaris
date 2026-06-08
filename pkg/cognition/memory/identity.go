package memory

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// defaultPolarisIdentityFallback 是极简兜底文本。
// 正常路径从 configs/prompts/identity.md（embedded）加载；
// 用户层从 ~/.polarisagi/polaris/config/prompts/identity.md 加载（优先级更高）。
// 只有两路都失败时才使用此常量，保证系统始终可用。
const defaultPolarisIdentityFallback = "你是 Polaris，一个开源自托管 AI Agent。你直接高效，有工具时立即调用。"

// toolUseEnforcementGuidanceFallback 极简兜底，不替代 embedded 文件。
const toolUseEnforcementGuidanceFallback = "有工具可用时必须立即调用，禁止仅输出执行计划或说明性描述。"

// toolUseEnforcementModels 需要注入工具调用强制引导的模型名称子串（小写匹配）。
var toolUseEnforcementModels = []string{
	"deepseek", "qwen", "glm", "gpt", "codex", "grok", "gemini", "gemma",
}

// embeddedPromptsFS 由 server 层（pkg/gateway/）在启动时注入。
// 分离原因：pkg/cognition/memory/ 禁止直接 import configs 包（依赖方向约束）。
// configs.FS 由 M13 Interface 层通过 memory.SetEmbeddedPrompts() 传入。
var embeddedPromptsFS fs.FS

// userConfigDir 由 server 启动时通过 SetConfigDir 注入（DataLayout.Config）。
// 默认回退到 ~/.polarisagi/polaris/config（兼容未注入场景）。
var userConfigDir string

// SetEmbeddedPrompts 由 server 启动时注入 configs.FS，供 ReadPrompt 使用。
// 必须在 NewServer() 之前调用，否则 embedded 路径不可用（回退到 fallback 常量）。
func SetEmbeddedPrompts(fsys fs.FS) {
	embeddedPromptsFS = fsys
}

// SetConfigDir 由 server 启动时注入 DataLayout.Config，消除 identity/prompt 路径的 hardcode home。
// 必须在首次调用 ReadPrompt / LoadSoulMD / WriteUserPrompt 之前完成。
func SetConfigDir(dir string) {
	userConfigDir = dir
}

// resolveConfigDir 返回运行时配置目录（注入值优先，兜底 home 路径）。
func resolveConfigDir() string {
	if userConfigDir != "" {
		return userConfigDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".polarisagi/polaris", "config")
}

// ReadPrompt 按三所有权层优先级读取提示词文件内容。
//
// 优先级（高→低）：
//  1. 用户文件 ~/.polarisagi/polaris/config/prompts/{name}（用户资产，DB 重置不影响）
//  2. Embedded 默认 configs/prompts/{name}（随二进制发布，代码 PR 才能改）
//  3. fallback 参数（极简硬编码，应对 embedded 加载失败）
func ReadPrompt(name, fallback string) string {
	// Layer 1: 用户文件（优先）
	if content := loadUserPromptFile(name); content != "" {
		return content
	}
	// Layer 0: embedded 默认
	if content := loadEmbeddedPrompt("prompts/" + name); content != "" {
		return content
	}
	// 兜底
	if fallback != "" {
		return fallback
	}
	return defaultPolarisIdentityFallback
}

// DefaultIdentity 返回当前生效的 Agent 身份文本（三层优先级）。
func DefaultIdentity() string {
	return ReadPrompt("identity.md", defaultPolarisIdentityFallback)
}

// NeedsToolUseEnforcement 判断指定模型是否需要注入工具调用强制引导。
func NeedsToolUseEnforcement(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, pattern := range toolUseEnforcementModels {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// ModelSpecificGuidance 返回 modelID 对应的模型专属引导文本。
// 空字符串表示无需注入。
func ModelSpecificGuidance(modelID string) string {
	lower := strings.ToLower(modelID)
	switch {
	case containsAny(lower, "deepseek", "qwen", "glm"):
		return ReadPrompt("tool_enforcement/deepseek.md", toolUseEnforcementGuidanceFallback)
	case containsAny(lower, "gpt", "codex", "grok"):
		return ReadPrompt("tool_enforcement/openai.md", toolUseEnforcementGuidanceFallback)
	case containsAny(lower, "gemini", "gemma"):
		return ReadPrompt("tool_enforcement/google.md", toolUseEnforcementGuidanceFallback)
	}
	return ""
}

// LoadSoulMD 加载用户自定义身份文件。
// 优先读 config/prompts/identity.md；兼容旧路径 config/SOUL.md。
func LoadSoulMD() string {
	if content := loadUserPromptFile("identity.md"); content != "" {
		return content
	}
	// 兼容旧路径 SOUL.md
	b, err := os.ReadFile(filepath.Join(resolveConfigDir(), "SOUL.md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteUserPrompt 将用户编辑的提示词写入 config/prompts/{name}。
// name 只允许 identity.md 和 custom_instructions.md 两个值（安全限制）。
// 调用方负责校验 name 合法性（见 server/prompts.go allowedUserPrompts）。
func WriteUserPrompt(name, content string) error {
	dir := filepath.Join(resolveConfigDir(), "prompts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
}

// DeleteUserPrompt 删除用户自定义提示词文件，恢复到 embedded 默认。
// 文件不存在时静默返回 nil（幂等）。
func DeleteUserPrompt(name string) error {
	path := filepath.Join(resolveConfigDir(), "prompts", name)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ReadPromptDefault 只读取 embedded Layer 0 的默认值，忽略用户文件。
// 用于 API 响应中展示"内置默认值"。
func ReadPromptDefault(name string) string {
	if content := loadEmbeddedPrompt("prompts/" + name); content != "" {
		return content
	}
	return defaultPolarisIdentityFallback
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func loadUserPromptFile(name string) string {
	b, err := os.ReadFile(filepath.Join(resolveConfigDir(), "prompts", name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func loadEmbeddedPrompt(name string) string {
	if embeddedPromptsFS == nil {
		return ""
	}
	b, err := fs.ReadFile(embeddedPromptsFS, name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
