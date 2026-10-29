package adapter

import "github.com/polarisagi/polaris/pkg/types"

// InspectChatRequest 把消息与调用选项翻译成将要发送的 OpenAI 兼容请求体（不含鉴权、不发网络）。
// 前缀稳定门控（ADR-0120 决策一）用它复用生产翻译逻辑：缓存按 provider 实际收到的字节前缀匹配，
// 测试若自造一套序列化，就会在翻译层的改动面前失明。
func InspectChatRequest(msgs []types.Message, opts ...types.InferOption) *OpenAIRequest {
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return translateRequest(&types.InferRequest{
		Messages:       msgs,
		Model:          options.Model,
		MaxTokens:      options.MaxTokens,
		Tools:          options.Tools,
		ToolChoice:     options.ToolChoice,
		ThinkingMode:   options.ThinkingMode,
		Temperature:    options.Temperature,
		ResponseFormat: options.ResponseFormat,
	}, false)
}
