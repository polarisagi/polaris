package prompt

import "context"

// PromptFacade 提示词管理模块对外统一接口。
//
// 问题背景：
//
//	当前 prompt.Manager 被多个上层模块直接持有（*prompt.Manager），
//	gateway/server 持有、agent 持有、learning/engine 持有，三方都 import prompt 包。
//	未来 prompt 包引入持久化（optimizer 写入 DB）时，容易引发循环依赖。
//
// 解决方案：
//   - PromptFacade 是 prompt 包对外的统一入口接口
//   - 上层模块依赖此接口，不直接持有 *Manager
//   - optimizer 子包实现通过 SetOptimizer 注入，不反向 import 上层
//
// @consumer: agent/agent.go, gateway/server/server.go, learning/engine.go
// @producer: prompt.Manager（由 cli.go/bootstrap 构造注入）
type PromptFacade interface {
	// ReadPrompt 读取提示词（优先用户自定义，回退内置嵌入文件）。
	ReadPrompt(name, fallback string) string

	// WriteUserPrompt 持久化用户自定义提示词（写入 configDir）。
	WriteUserPrompt(name, content string) error

	// DeleteUserPrompt 删除用户自定义提示词（回退到内置）。
	DeleteUserPrompt(name string) error

	// DefaultIdentity 返回系统默认身份提示词（${POLARIS_IDENTITY} 变量）。
	DefaultIdentity() string

	// GetSoulMD 加载 SOUL.md（用户定制人格文件，启动时一次性读取）。
	GetSoulMD() string

	// PlatformHintFor 返回接入平台感知提示词片段（cli/webui/api/cron）。
	PlatformHintFor(platform string) string

	// Optimize 异步优化指定 task_type 的 system prompt（Eval Harness 反馈驱动）。
	// 优化结果写入 DB prompt_versions 表，通过 ActivateCallback 热更新。
	Optimize(ctx context.Context, taskType string) error
}

// ActivateCallback 当新版本 system prompt 激活时的回调（热更新）。
// 实现：gateway/server 注册此回调，learning/engine 触发调用。
type ActivateCallback func(taskType, newPrompt string)
