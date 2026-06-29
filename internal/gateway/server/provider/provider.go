package provider

import "github.com/polarisagi/polaris/internal/protocol"

// 本文件声明 provider 包对外部模块的消费端接口（Consumer-side Interfaces）。
// 方法集以 loader.go + providers.go 实际调用点为准。
//
// @consumer: handler.go（字段类型），loader.go（函数参数类型）
// @producer: 具体实现由 llm.ProviderRegistry 满足，由 cmd/polaris/boot_server.go 注入

// ProviderRegistry provider 包对 LLM 注册表的消费端接口。
// 实现：llm.ProviderRegistry
type ProviderRegistry interface {
	// UnregisterAll 清空当前所有已注册 Provider（重新从 DB 加载前调用）。
	UnregisterAll()
	// RegisterWithRole 注册一个 Provider，绑定角色（default / general / reasoning）。
	RegisterWithRole(name, displayName, role string, p protocol.Provider)
}
