// Package hook 实现两家共同的 hooks.json 生命周期钩子模型（ADR-0103 决策六）。
//
// 配置：{"hooks": {"<Event>": [{"matcher": "<regex>", "hooks": [<handler>...]}]}}；来源为
// 用户级 / 项目级 hooks.json 与已安装插件（manifest 快照）。插件与项目级来源按定义内容
// 哈希审阅信任后才执行（安装 ≠ 信任，Codex 规则）。
// 协议：输入 JSON 经 stdin 传入；退出码 0 = 成功（stdout 可为 JSON 决策），2 = 阻断
// （原因取 stderr），其他 = 非阻断错误。Hook 输出强制 TaintLevel=High，不得进入
// System Prompt 不可变区（ADR-0016 决策二）。
package hook

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Event 钩子事件名（两家标准名称，大小写敏感）。
type Event string

const (
	EventSessionStart       Event = "SessionStart"
	EventSessionEnd         Event = "SessionEnd"
	EventUserPromptSubmit   Event = "UserPromptSubmit"
	EventPreToolUse         Event = "PreToolUse"
	EventPermissionRequest  Event = "PermissionRequest"
	EventPostToolUse        Event = "PostToolUse"
	EventPostToolUseFailure Event = "PostToolUseFailure"
	EventSubagentStart      Event = "SubagentStart"
	EventSubagentStop       Event = "SubagentStop"
	EventStop               Event = "Stop"
	EventStopFailure        Event = "StopFailure"
	EventPreCompact         Event = "PreCompact"
	EventPostCompact        Event = "PostCompact"
	EventNotification       Event = "Notification"
	EventElicitation        Event = "Elicitation"
	EventElicitationResult  Event = "ElicitationResult"
	EventInterrupt          Event = "Interrupt"
)

// SupportedEvents Polaris 有对应语义并会触发的事件。其余标准事件（Worktree*、CwdChanged、
// FileChanged、InstructionsLoaded、MessageDisplay 等 IDE/终端事件）解析但不触发。
func SupportedEvents() []Event {
	return []Event{EventSessionStart, EventSessionEnd, EventUserPromptSubmit, EventPreToolUse, EventPermissionRequest,
		EventPostToolUse, EventPostToolUseFailure, EventSubagentStart, EventSubagentStop, EventStop, EventStopFailure,
		EventPreCompact, EventPostCompact, EventNotification, EventElicitation, EventElicitationResult, EventInterrupt}
}

// 处理器类型。
const (
	TypeCommand = "command"
	TypeHTTP    = "http"
	TypeMCPTool = "mcp_tool"
	TypePrompt  = "prompt"
	TypeAgent   = "agent"
)

const defaultTimeout = 600 * time.Second

// Handler 单个处理器（两家字段的并集）。
type Handler struct {
	Type           string            `json:"type"`
	Command        string            `json:"command,omitempty"`
	CommandWindows string            `json:"commandWindows,omitempty"`
	Args           []string          `json:"args,omitempty"` // exec 形式：Command 为可执行文件
	Async          bool              `json:"async,omitempty"`
	TimeoutSec     int               `json:"timeout,omitempty"`
	StatusMessage  string            `json:"statusMessage,omitempty"`
	If             string            `json:"if,omitempty"` // 权限规则语法，如 "Bash(git *)"
	Once           bool              `json:"once,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	AllowedEnvVars []string          `json:"allowedEnvVars,omitempty"`
	Server         string            `json:"server,omitempty"`
	Tool           string            `json:"tool,omitempty"`
	Input          map[string]any    `json:"input,omitempty"`
	Prompt         string            `json:"prompt,omitempty"`
	Model          string            `json:"model,omitempty"`
	// AdditionalContextLimit Codex：additionalContext 超过该 token 量时溢出为可取回引用。
	AdditionalContextLimit int `json:"additionalContextLimit,omitempty"`
}

// Timeout 处理器超时（缺省 600s，两家一致）。
func (h Handler) Timeout() time.Duration {
	if h.TimeoutSec <= 0 {
		return defaultTimeout
	}
	return time.Duration(h.TimeoutSec) * time.Second
}

// MatcherGroup 一个事件下的匹配组。
type MatcherGroup struct {
	Matcher  string    `json:"matcher,omitempty"`
	Hooks    []Handler `json:"hooks"`
	compiled *regexp.Regexp
}

// Matches 空、"*" 匹配全部；否则按正则匹配（两家均为正则；非法正则退化为精确匹配）。
func (g *MatcherGroup) Matches(subject string) bool {
	if g.Matcher == "" || g.Matcher == "*" {
		return true
	}
	if g.compiled != nil {
		return g.compiled.MatchString(subject)
	}
	return g.Matcher == subject
}

// Config 事件 → 匹配组。
type Config map[Event][]MatcherGroup

// ParseEvents 解析 "事件名 → 匹配组数组" 原始 JSON（pluginspec.HookSource.Events 或 hooks.json 的 hooks 值）。
func ParseEvents(events map[string]json.RawMessage) (Config, error) {
	cfg := Config{}
	for name, raw := range events {
		var groups []MatcherGroup
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "hook: event "+name, err)
		}
		for i := range groups {
			if err := validateGroup(&groups[i]); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "hook: event "+name, err)
			}
		}
		cfg[Event(name)] = groups
	}
	return cfg, nil
}

// ParseFile 解析 hooks.json 文件内容：{"hooks": {...}}（可含 description）。
func ParseFile(raw []byte) (Config, error) {
	var top struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "hook: parse hooks.json", err)
	}
	return ParseEvents(top.Hooks)
}

func validateGroup(g *MatcherGroup) error {
	if g.Matcher != "" && g.Matcher != "*" {
		if re, err := regexp.Compile("^(?:" + g.Matcher + ")$"); err == nil {
			g.compiled = re
		}
	}
	for _, h := range g.Hooks {
		if err := validateHandler(h); err != nil {
			return err
		}
	}
	return nil
}

func validateHandler(h Handler) error {
	var missing string
	switch h.Type {
	case TypeCommand:
		if strings.TrimSpace(h.Command) == "" {
			missing = "command"
		}
	case TypeHTTP:
		if h.URL == "" {
			missing = "url"
		}
	case TypeMCPTool:
		if h.Server == "" || h.Tool == "" {
			missing = "server/tool"
		}
	case TypePrompt, TypeAgent:
		if h.Prompt == "" {
			missing = "prompt"
		}
	default:
		return apperr.New(apperr.CodeInvalidInput, "unsupported hook handler type "+h.Type)
	}
	if missing != "" {
		return apperr.New(apperr.CodeInvalidInput, h.Type+" hook requires "+missing)
	}
	return nil
}
