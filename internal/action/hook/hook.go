// Package hook 实现 ShellHook 生命周期注入框架（[ShellHooks] 设计意图落地）。
// 触发点：SessionStart / PreToolUse / PostToolUse / UserPromptSubmit / Stop。
// 输出强制 TaintLevel=High，通过 M11 PolicyGate 后才可注入 Agent 上下文。
package hook

import (
	"regexp"
	"time"
)

// Event 枚举 Hook 触发事件类型。
type Event string

const (
	EventSessionStart     Event = "SessionStart"
	EventPreToolUse       Event = "PreToolUse"
	EventPostToolUse      Event = "PostToolUse"
	EventUserPromptSubmit Event = "UserPromptSubmit"
	EventStop             Event = "Stop"
)

// HandlerConfig 单个 Hook 处理器配置。
type HandlerConfig struct {
	Type          string        // 当前只支持 "command"
	Command       string        // shell 命令（在 session cwd 执行）
	StatusMessage string        // UI 状态提示（可选）
	Timeout       time.Duration // 超时，默认 30s
}

// MatcherGroup 一个事件下的匹配组（matcher + 处理器列表）。
type MatcherGroup struct {
	// Matcher 正则，匹配工具名（PreToolUse/PostToolUse 专用）。
	// 空字符串 = 匹配所有。
	Matcher  string
	compiled *regexp.Regexp
	Hooks    []HandlerConfig
}

// Config Hook 配置文件结构（hooks.yaml 根对象）。
type Config struct {
	Hooks map[Event][]MatcherGroup `yaml:"hooks"`
}

// HookInput 传递给 hook 脚本的上下文（写入 stdin JSON）。
type HookInput struct {
	Event     Event             `json:"event"`
	ToolName  string            `json:"tool_name,omitempty"`
	ToolInput map[string]any    `json:"tool_input,omitempty"`
	Output    string            `json:"output,omitempty"` // PostToolUse 时工具输出
	SessionID string            `json:"session_id"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// HookResult hook 脚本执行结果。
type HookResult struct {
	Event      Event
	Handler    string // Command 摘要
	ExitCode   int
	Stdout     string // TaintLevel=High 封装前的原始输出
	Stderr     string
	DurationMs int64
	Err        error
}
