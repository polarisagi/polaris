package protocol

// ImmutableCoreFields 是系统提示词组装的可变字段集合（M05 §2.2 ImmutableCore）。
//
// producer: internal/memory/store.ImmutableCore（内嵌本结构体，Load/PrependToMessages/
// renderSystemPrompt 等业务逻辑保留在 memory/store）
// consumer: internal/gateway（chat/sse.go 每轮请求组装 stable/volatile 层字段、
// sysadmin/preferences.go 热更新用户偏好）
//
// 此前 gateway 通过 `.(*store.ImmutableCore)` 类型断言绕过 protocol.ImmutableCore
// 接口直接读写这些字段，违反 M04 §B2。现将可写字段集合收敛至此，
// protocol.ImmutableCore.Fields() 返回本结构体指针供调用方读写，
// 不再需要下探到 memory/store 具体类型。
type ImmutableCoreFields struct {
	AgentName            string            `json:"agent_name"`
	AgentRole            string            `json:"agent_role"`
	ModelID              string            `json:"model_id"`
	BuiltinTools         string            `json:"builtin_tools"`
	InstalledPlugins     string            `json:"installed_plugins"`
	UserPreferences      map[string]string `json:"user_preferences"`
	GlobalGoal           string            `json:"global_goal"`
	SystemPromptTemplate string            `json:"system_prompt_template"`

	// 三层系统提示词组装字段（stable + volatile）

	// SoulMDContent 用户自定义身份文件内容（~/.polarisagi/polaris/config/SOUL.md）。
	// 非空时替换 DefaultPolarisIdentity 作为 stable 层首段。
	SoulMDContent string `json:"soul_md_content,omitempty"`

	// ModelGuidance 模型专属工具调用引导，由 M13 Interface 层按 ModelID 注入到 stable 层。
	ModelGuidance string `json:"model_guidance,omitempty"`

	// PlatformHint 平台感知提示词，由 M13 Interface 层按接入平台注入到 stable 层末尾。
	PlatformHint string `json:"platform_hint,omitempty"`

	// VolatileBlock 易变信息区（时间戳/会话 ID/模型信息），每轮刷新。
	VolatileBlock string `json:"volatile_block,omitempty"`

	// AmbientContext ambient skill 上下文（instructions + 目录索引行）。
	AmbientContext string `json:"ambient_context,omitempty"`

	// CustomInstructions 用户追加的行为指令（stable 层末尾，追加而非覆盖身份）。
	CustomInstructions string `json:"custom_instructions,omitempty"`

	// UserProfile L3 用户画像摘要（StableFacts + 高频 BehavioralPatterns）。
	UserProfile string `json:"user_profile,omitempty"`

	// OperationalDirectives 高级操作指令集合（含 Tool-Use, Task Completion 等）。
	OperationalDirectives string `json:"operational_directives,omitempty"`
}
