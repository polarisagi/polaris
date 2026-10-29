package adapter

import (
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

type anthropicPayload struct {
	System     []map[string]any `json:"system"`
	Tools      []map[string]any `json:"tools"`
	ToolChoice map[string]any   `json:"tool_choice"`
	Messages   []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func buildPayload(t *testing.T, a *AnthropicAdapter, req *types.InferRequest) anthropicPayload {
	t.Helper()
	raw, err := a.buildAnthropicRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var p anthropicPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// msgHasBreakpoint 判断消息是否在某个 content block 上带 cache_control。
func msgHasBreakpoint(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, b := range blocks {
		if bm, ok := b.(map[string]any); ok && bm["cache_control"] != nil {
			return true
		}
	}
	return false
}

func countBreakpoints(p anthropicPayload) int {
	n := 0
	for _, b := range p.System {
		if b["cache_control"] != nil {
			n++
		}
	}
	for _, tl := range p.Tools {
		if tl["cache_control"] != nil {
			n++
		}
	}
	for _, m := range p.Messages {
		if msgHasBreakpoint(m.Content) {
			n++
		}
	}
	return n
}

func cachingAdapter(opts ...AnthropicOption) *AnthropicAdapter {
	a := &AnthropicAdapter{model: "claude-sonnet-5"}
	WithAnthropicPromptCaching()(a)
	for _, o := range opts {
		o(a)
	}
	return a
}

// ADR-0105 决策三：有 CacheBreakpoint 标记时，断点落在 L0 首块 system、L2 末（标记消息）、最后一条消息，
// 不再给末个 system block 与倒数第二条消息占名额。
func TestBuildAnthropicRequest_HonorsCacheBreakpointFlag(t *testing.T) {
	req := &types.InferRequest{Messages: []types.Message{
		{Role: "system", Content: "L0 stable"},
		{Role: "system", Content: "L1 session"},
		{Role: "user", Content: "h1"},
		{Role: "assistant", Content: "h2"},
		{Role: "user", Content: "h3", CacheBreakpoint: true}, // L2 末
		{Role: "system", Content: "L3 phase"},
		{Role: "user", Content: "now"},
	}}
	p := buildPayload(t, cachingAdapter(), req)

	if p.System[0]["cache_control"] == nil {
		t.Fatal("L0 首块 system 必须带断点")
	}
	if p.System[1]["cache_control"] != nil || p.System[2]["cache_control"] != nil {
		t.Fatalf("有标记时末个 system block 不应占名额：%v", p.System)
	}
	want := []bool{false, false, true, true} // h1,h2,h3(flag),now(last)
	for i, m := range p.Messages {
		if got := msgHasBreakpoint(m.Content); got != want[i] {
			t.Fatalf("msg[%d] 断点=%v，期望 %v", i, got, want[i])
		}
	}
	if n := countBreakpoints(p); n != 3 {
		t.Fatalf("断点总数应为 3，got %d", n)
	}
}

// 标记消息恰为最后一条时不重复占名额。
func TestBuildAnthropicRequest_FlagOnLastMessageDedup(t *testing.T) {
	req := &types.InferRequest{Messages: []types.Message{
		{Role: "system", Content: "L0"},
		{Role: "user", Content: "a"},
		{Role: "user", Content: "b", CacheBreakpoint: true},
	}}
	p := buildPayload(t, cachingAdapter(), req)
	if n := countBreakpoints(p); n != 2 {
		t.Fatalf("应为 L0 + 末消息共 2 个，got %d", n)
	}
}

// 标记过多时总数仍 ≤4，且优先保留最靠后的标记。
func TestBuildAnthropicRequest_FlagsCappedAtFour(t *testing.T) {
	var msgs []types.Message
	msgs = append(msgs, types.Message{Role: "system", Content: "L0"})
	for i := 0; i < 6; i++ {
		msgs = append(msgs, types.Message{Role: "user", Content: "u", CacheBreakpoint: true})
	}
	p := buildPayload(t, cachingAdapter(), &types.InferRequest{Messages: msgs})
	if n := countBreakpoints(p); n != 4 {
		t.Fatalf("断点应恰好 4 个（上限），got %d", n)
	}
	if !msgHasBreakpoint(p.Messages[len(p.Messages)-1].Content) || msgHasBreakpoint(p.Messages[0].Content) {
		t.Fatal("应保留最靠后的标记，丢弃最早的")
	}
}

// 无 system、有 tools：L0 断点退回 tools 末尾，再加标记消息与末消息。
func TestBuildAnthropicRequest_NoSystemFallsBackToToolsWithFlag(t *testing.T) {
	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "user", Content: "a", CacheBreakpoint: true},
			{Role: "assistant", Content: "b"},
			{Role: "user", Content: "c"},
		},
		Tools: []types.ToolSchema{{Name: "a"}, {Name: "b"}},
	}
	p := buildPayload(t, cachingAdapter(), req)
	if p.Tools[1]["cache_control"] == nil || p.Tools[0]["cache_control"] != nil {
		t.Fatalf("tools 末尾应带断点：%v", p.Tools)
	}
	if !msgHasBreakpoint(p.Messages[0].Content) || msgHasBreakpoint(p.Messages[1].Content) || !msgHasBreakpoint(p.Messages[2].Content) {
		t.Fatal("标记消息与末消息应带断点，中间消息不带")
	}
	if n := countBreakpoints(p); n != 3 {
		t.Fatalf("got %d", n)
	}
}

// 无标记时保持旧行为：首块 + 末块 system + 最近 2 条消息。
func TestBuildAnthropicRequest_NoFlagKeepsLegacyHeuristic(t *testing.T) {
	req := &types.InferRequest{Messages: []types.Message{
		{Role: "system", Content: "L0"},
		{Role: "system", Content: "tail"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}}
	p := buildPayload(t, cachingAdapter(), req)
	if p.System[0]["cache_control"] == nil || p.System[1]["cache_control"] == nil {
		t.Fatal("首/末 system 必须带断点")
	}
	if msgHasBreakpoint(p.Messages[0].Content) || !msgHasBreakpoint(p.Messages[1].Content) || !msgHasBreakpoint(p.Messages[2].Content) {
		t.Fatal("应只标记最近 2 条消息")
	}
	if n := countBreakpoints(p); n != 4 {
		t.Fatalf("got %d", n)
	}
}

// TTL：默认不下发 ttl（API 默认 5m）；配置 1h 时所有断点带 ttl=1h；非法值被忽略。
func TestBuildAnthropicRequest_CacheTTL(t *testing.T) {
	req := &types.InferRequest{Messages: []types.Message{
		{Role: "system", Content: "L0"},
		{Role: "user", Content: "hi"},
	}}
	def := buildPayload(t, cachingAdapter(WithAnthropicCacheTTL("5m")), req)
	if cc, _ := def.System[0]["cache_control"].(map[string]any); cc == nil || cc["ttl"] != nil {
		t.Fatalf("5m 不应显式下发 ttl：%v", def.System[0]["cache_control"])
	}
	long := buildPayload(t, cachingAdapter(WithAnthropicCacheTTL("1h")), req)
	if cc, _ := long.System[0]["cache_control"].(map[string]any); cc["ttl"] != "1h" {
		t.Fatalf("1h 应下发 ttl：%v", long.System[0]["cache_control"])
	}
	msgBlocks := long.Messages[0].Content.([]any)
	if cc := msgBlocks[0].(map[string]any)["cache_control"].(map[string]any); cc["ttl"] != "1h" {
		t.Fatalf("消息断点也应带 1h：%v", cc)
	}
	bad := buildPayload(t, cachingAdapter(WithAnthropicCacheTTL("9h")), req)
	if cc, _ := bad.System[0]["cache_control"].(map[string]any); cc["ttl"] != nil {
		t.Fatal("非法 TTL 应被忽略")
	}
}

// 同一会话同一输入两次构造的请求体字节一致（tools 顺序、thinking 参数确定）。
func TestBuildAnthropicRequest_DeterministicBytes(t *testing.T) {
	req := &types.InferRequest{
		Messages:     []types.Message{{Role: "system", Content: "L0"}, {Role: "user", Content: "hi"}},
		Tools:        []types.ToolSchema{{Name: "b", Parameters: map[string]any{"type": "object", "properties": map[string]any{"z": 1, "a": 2}}}, {Name: "a"}},
		ThinkingMode: types.ThinkingHigh,
	}
	a := cachingAdapter()
	first, err := a.buildAnthropicRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, _ := a.buildAnthropicRequest(req, false)
		if string(got) != string(first) {
			t.Fatal("同输入请求体字节必须一致")
		}
	}
}

func TestBuildAnthropicRequest_ToolChoice(t *testing.T) {
	tools := []types.ToolSchema{{Name: "a"}}
	msgs := []types.Message{{Role: "user", Content: "hi"}}
	p := buildPayload(t, cachingAdapter(), &types.InferRequest{Messages: msgs, Tools: tools, ToolChoice: "none"})
	if p.ToolChoice["type"] != "none" {
		t.Fatalf("tool_choice 应为 none：%v", p.ToolChoice)
	}
	p = buildPayload(t, cachingAdapter(), &types.InferRequest{Messages: msgs, Tools: tools})
	if p.ToolChoice != nil {
		t.Fatal("未设置时不应下发 tool_choice")
	}
	// 无 tools 时 tool_choice 会被 API 拒绝，必须不发。
	p = buildPayload(t, cachingAdapter(), &types.InferRequest{Messages: msgs, ToolChoice: "none"})
	if p.ToolChoice != nil {
		t.Fatal("无 tools 时不应下发 tool_choice")
	}
}
