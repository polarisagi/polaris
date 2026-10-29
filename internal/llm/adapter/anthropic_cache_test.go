package adapter

import (
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// ADR-0102 决策三：多 system 消息保留边界，首块（ImmutableCore 稳定前缀）与末块各一个断点；
// 断点总数不超过 Anthropic 上限 4。
func TestBuildAnthropicRequest_StablePrefixBreakpoint(t *testing.T) {
	a := &AnthropicAdapter{model: "claude-sonnet-5"}
	WithAnthropicPromptCaching()(a)
	req := &types.InferRequest{Messages: []types.Message{
		{Role: "system", Content: "PERSONA (stable)"},
		{Role: "system", Content: "# TASK PERCEPTION"},
		{Role: "system", Content: "<core_memory>volatile</core_memory>"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "list files"},
	}, Tools: []types.ToolSchema{{Name: "ls"}}}

	raw, err := a.buildAnthropicRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		System   []map[string]any `json:"system"`
		Tools    []map[string]any `json:"tools"`
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.System) != 3 {
		t.Fatalf("system 应保留 3 个 block，got %d", len(payload.System))
	}
	if payload.System[0]["text"] != "PERSONA (stable)" || payload.System[0]["cache_control"] == nil {
		t.Fatalf("首个 system block 必须带断点（稳定前缀）：%v", payload.System[0])
	}
	if payload.System[1]["cache_control"] != nil {
		t.Fatalf("中间 block 不应占用断点：%v", payload.System[1])
	}
	if payload.System[2]["cache_control"] == nil {
		t.Fatalf("末个 system block 必须带断点：%v", payload.System[2])
	}
	breakpoints := 0
	for _, b := range payload.System {
		if b["cache_control"] != nil {
			breakpoints++
		}
	}
	for _, tl := range payload.Tools {
		if tl["cache_control"] != nil {
			breakpoints++
		}
	}
	for _, m := range payload.Messages {
		if blocks, ok := m.Content.([]any); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok && bm["cache_control"] != nil {
					breakpoints++
				}
			}
		}
	}
	if breakpoints > 4 {
		t.Fatalf("Anthropic 最多 4 个断点，got %d", breakpoints)
	}
}
