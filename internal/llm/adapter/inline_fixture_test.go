package adapter

import "github.com/polarisagi/polaris/pkg/types"

// 内联 system 测试的共享请求样例（仅依赖 types，便于在改动前后的代码上生成/比对黄金输出）。

// fixtureMidSystemRequest 含中部 system、工具调用往返与断点置位，用于"关闭内联 = 与改动前字节一致"回归。
func fixtureMidSystemRequest() *types.InferRequest {
	return &types.InferRequest{
		Model:     "claude-test-model",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "system", Content: "core persona", CacheBreakpoint: true},
			{Role: "system", Content: "session env"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", Parts: []any{
				map[string]any{"type": "text", "text": "calling"},
				map[string]any{"type": "tool_use", "id": "t1", "name": "get_weather", "input": map[string]any{"city": "SH"}},
			}},
			{Role: "user", Parts: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "name": "get_weather", "content": "{\"temp\":20}"},
			}},
			{Role: "assistant", Content: "sunny", CacheBreakpoint: true},
			{Role: "system", Content: "phase template"},
			{Role: "user", Content: "next question"},
		},
	}
}

// fixtureFiveLayerRequest 五层账本样例：L0 system(断点) / L1 system / L2 user+assistant(末条断点) / L3 system / L4 user。
func fixtureFiveLayerRequest() *types.InferRequest {
	return &types.InferRequest{
		Model:     "claude-test-model",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "system", Content: "L0 stable core", CacheBreakpoint: true},
			{Role: "system", Content: "L1 session env"},
			{Role: "user", Content: "L2 user-1"},
			{Role: "assistant", Content: "L2 assistant-1"},
			{Role: "user", Content: "L2 user-2"},
			{Role: "assistant", Content: "L2 assistant-2", CacheBreakpoint: true},
			{Role: "system", Content: "L3 phase template"},
			{Role: "user", Content: "L4 turn input"},
		},
	}
}
