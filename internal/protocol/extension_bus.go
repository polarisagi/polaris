package protocol

import "context"

// ExtensionBus 扩展统一总线接口（消费方定义）。
// @consumer: M4(Agent Kernel), M13(Gateway/Plugin API)
// @producer: internal/extension/bus/
type ExtensionBus interface {
	Install(ctx context.Context, req any) error
	Activate(ctx context.Context, goal string) ([]ActivatedHint, error)
	ListInstalled(ctx context.Context) ([]any, error)
	Uninstall(ctx context.Context, catalogID string) error
}

// ActivatedHint 已激活扩展的工具提示（避免引入 native 包）。
type ActivatedHint struct {
	ExtensionID string
	ToolName    string
	Description string
}
