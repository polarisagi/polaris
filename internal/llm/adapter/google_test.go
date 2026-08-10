package adapter

import (
	"testing"
)

// TestBuildInferResponseFromGemini_FunctionCallOnlyNotEmpty 复现修复前的缺陷：
// Gemini 响应若仅含 functionCall part（无 text part，模型直接发起工具调用而不
// 附带任何说明文字，常见于强制 tool_choice 场景），buildInferResponseFromGemini
// 此前完全忽略 p.FunctionCall，导致 resp.Content == "" 且 resp.ToolCalls 为空，
// 命中"空响应"兜底误报 apperr（llm: empty response from provider）。
func TestBuildInferResponseFromGemini_FunctionCallOnlyNotEmpty(t *testing.T) {
	a := &GoogleAgentPlatformAdapter{model: "gemini-test"}

	out := &geminiInferResponse{
		Candidates: []struct {
			Content struct {
				Parts []struct {
					Text         string              `json:"text"`
					FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		}{
			{
				FinishReason: "STOP",
				Content: struct {
					Parts []struct {
						Text         string              `json:"text"`
						FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
					} `json:"parts"`
				}{
					Parts: []struct {
						Text         string              `json:"text"`
						FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
					}{
						{
							FunctionCall: &geminiFunctionCall{
								Name: "get_weather",
								Args: map[string]any{"city": "Shanghai"},
							},
						},
					},
				},
			},
		},
	}

	resp, err := a.buildInferResponseFromGemini(out, "gemini-test")
	if err != nil {
		t.Fatalf("buildInferResponseFromGemini returned unexpected error for functionCall-only response: %v", err)
	}
	if resp.Content != "" {
		t.Errorf("expected empty Content for functionCall-only response, got %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected exactly 1 ToolCall, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("expected ToolCall.Name = get_weather, got %q", tc.Name)
	}
	if tc.ID == "" {
		t.Errorf("expected non-empty ToolCall.ID")
	}
	if len(tc.Input) == 0 {
		t.Errorf("expected non-empty ToolCall.Input (marshaled args)")
	}
}

// TestBuildInferResponseFromGemini_TrulyEmptyStillErrors 确保回归修复不破坏原有
// 兜底行为：既无 text 也无 functionCall 的响应仍应报错，而不是静默放行空回复。
func TestBuildInferResponseFromGemini_TrulyEmptyStillErrors(t *testing.T) {
	a := &GoogleAgentPlatformAdapter{model: "gemini-test"}
	out := &geminiInferResponse{}

	_, err := a.buildInferResponseFromGemini(out, "gemini-test")
	if err == nil {
		t.Fatalf("expected error for truly empty Gemini response, got nil")
	}
}
