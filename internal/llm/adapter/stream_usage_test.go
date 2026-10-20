package adapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/pkg/types"
)

// sseBody 把 data 帧拼成 SSE 正文（每帧一个空行分隔）。
func sseBody(frames ...string) string {
	return strings.Join(frames, "\n\n") + "\n\n"
}

func sseClient(body string) *OpenAICompatibleClient {
	return &OpenAICompatibleClient{
		BaseURL: "http://dummy",
		HTTPClient: &http.Client{Transport: mockRoundTripperFunc(func(*http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
				Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
		})},
	}
}

type streamResult struct {
	text, thinking string
	toolCalls      []string
	usage          types.Usage
	usageEvents    int
}

func collect(ch <-chan types.StreamEvent) streamResult {
	var r streamResult
	for ev := range ch {
		switch ev.Type {
		case types.StreamTextDelta:
			r.text += ev.Content
		case types.StreamThinking:
			r.thinking += ev.Content
		case types.StreamToolCall:
			r.toolCalls = append(r.toolCalls, ev.Content)
		}
		if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
			r.usage = ev.Usage
			r.usageEvents++
		}
	}
	return r
}

// A：各家 usage 位置的表驱动覆盖。核心回归是 DeepSeek 风格——usage 放在带 finish_reason 的块内、
// delta.content=""、choices 非空；此前该块的 usage 被丢弃，llm_calls 的 token 恒为 0。
func TestSSEParser_UsagePlacement(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":120,"completion_tokens":34,"prompt_cache_hit_tokens":100}`
	cases := []struct {
		name      string
		frames    []string
		wantTools int
		wantText  string
	}{
		{"OpenAI 风格：末尾空 choices 块带 usage", []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"1","choices":[],` + usage + `}`,
			`data: [DONE]`}, 0, "你好"},
		{"DeepSeek 风格：finish=stop 块内带 usage 且 content 为空串", []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"想"},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"答案"},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],` + usage + `}`,
			`data: [DONE]`}, 0, "答案"},
		{"DeepSeek 风格：finish=tool_calls 块内带 usage", []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"要搜索"},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}],` + usage + `}`,
			`data: [DONE]`}, 1, ""},
		{"DeepSeek 风格：多 index 工具调用 + stop 收尾 + usage", []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{}"}},{"index":1,"id":"c2","function":{"name":"b","arguments":"{}"}}]},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":null},"finish_reason":"stop"}],` + usage + `}`,
			`data: [DONE]`}, 2, ""},
		{"usage 随正文块一起到达", []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],` + usage + `}`,
			`data: [DONE]`}, 0, "ok"},
		{"无 [DONE] 直接 EOF 仍补发 usage", []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],` + usage + `}`}, 0, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "hi"}}}
			ch, err := sseClient(sseBody(tc.frames...)).SendStreamRequest(ctx, nil, []byte("k"), translateRequest(req, true), 0)
			if err != nil {
				t.Fatal(err)
			}
			r := collect(ch)
			if r.usage.InputTokens != 120 || r.usage.OutputTokens != 34 || r.usage.CacheHitTokens != func() int {
				if strings.Contains(tc.frames[len(tc.frames)-1], "cache_hit") || strings.Contains(strings.Join(tc.frames, ""), "cache_hit") {
					return 100
				}
				return 0
			}() {
				t.Fatalf("usage 丢失或错误: %+v", r.usage)
			}
			if r.usageEvents != 1 {
				t.Fatalf("usage 应恰好随一个事件发出一次，实际 %d", r.usageEvents)
			}
			if len(r.toolCalls) != tc.wantTools || r.text != tc.wantText {
				t.Fatalf("tools=%d text=%q，期望 tools=%d text=%q", len(r.toolCalls), r.text, tc.wantTools, tc.wantText)
			}
		})
	}
}

// 无 usage 时不得凭空补发 usage 事件。
func TestSSEParser_NoUsageNoEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body := sseBody(`data: {"id":"1","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, `data: [DONE]`)
	ch, err := sseClient(body).SendStreamRequest(ctx, nil, []byte("k"), translateRequest(&types.InferRequest{}, true), 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := collect(ch); r.usageEvents != 0 || r.text != "x" {
		t.Fatalf("%+v", r)
	}
}

// deepSeekFixture 构造 DeepSeek 实际格式的长流：大量 reasoning 增量 → 多 index tool_calls →
// finish 块带 usage。reasoning 增量足够多，使下游慢消费时 relay 的 outCh 缓冲被填满。
func deepSeekFixture(thinkingFrames int) string {
	frames := make([]string, 0, thinkingFrames+6)
	for i := range thinkingFrames {
		frames = append(frames, fmt.Sprintf(`data: {"id":"1","choices":[{"index":0,"delta":{"content":null,"reasoning_content":"思考%d"},"finish_reason":null}]}`, i))
	}
	frames = append(frames,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"web_search","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"ai 赚钱\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":900,"completion_tokens":77,"prompt_cache_hit_tokens":512}}`,
		`data: [DONE]`)
	return sseBody(frames...)
}

// B2 根因回归：DeepSeek/OpenAI 适配器的 relay 协程此前在解码协程写完最后一帧、cancel() 之后，
// 以 `select { outCh<-ev; <-ctx.Done() }` 转发 rawCh 里尚未消费的尾部事件；两路同时就绪时
// Go 随机选择，约一半概率直接 return，丢掉 tool_calls 与末尾 usage——上游看到"只有思考、
// 无正文、无工具调用、output_tokens=0"。慢消费者 + 长思考流稳定放大该竞态。
func TestDeepSeekStreamInfer_TailEventsSurviveCancel(t *testing.T) {
	body := deepSeekFixture(400)
	client := &http.Client{Transport: mockRoundTripperFunc(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	})}
	adapter := NewDeepSeekAdapter(llmparent.NewCredentialPool([]string{"k"}, llmparent.StrategyFillFirst), client, "deepseek-flash", nil)

	for i := range 40 {
		// 不带 deadline 的 ctx：适配器内部才会派生可 cancel 的超时 ctx（真实守护进程即此形态）。
		ch, err := adapter.StreamInfer(context.Background(), []types.Message{{Role: "user", Content: "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		var r streamResult
		for ev := range ch {
			time.Sleep(20 * time.Microsecond) // 慢消费者
			switch ev.Type {
			case types.StreamThinking:
				r.thinking += ev.Content
			case types.StreamToolCall:
				r.toolCalls = append(r.toolCalls, ev.Content)
			}
			if ev.Usage.OutputTokens > 0 {
				r.usage = ev.Usage
			}
		}
		if len(r.toolCalls) != 2 || r.usage.OutputTokens != 77 || r.usage.CacheHitTokens != 512 {
			t.Fatalf("第 %d 次：尾部事件被丢弃 tool_calls=%d usage=%+v（thinking %d 字节）",
				i, len(r.toolCalls), r.usage, len(r.thinking))
		}
	}
}

// 变体：仅 reasoning 后 stop（真空输出）——适配器应如实给出思考链、无工具调用，并保留 usage，
// 由内核层的空输出恢复处理；不得吞掉也不得编造。
func TestSSEParser_ReasoningOnlyStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body := sseBody(
		`data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"Let me start with web_search calls."},"finish_reason":null}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`)
	ch, err := sseClient(body).SendStreamRequest(ctx, nil, []byte("k"), translateRequest(&types.InferRequest{}, true), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := collect(ch)
	if r.text != "" || len(r.toolCalls) != 0 || r.thinking == "" || r.usage.OutputTokens != 5 {
		t.Fatalf("%+v", r)
	}
}

// B1：POLARIS_LLM_RAW_DUMP 转储——写请求摘要与原始 SSE 行，绝不写密钥/消息正文。
func TestRawDump_WritesSSEAndNoSecrets(t *testing.T) {
	dir := t.TempDir()
	req := translateRequest(&types.InferRequest{
		Messages: []types.Message{{Role: "user", Content: "机密正文-不应落盘"}},
		Tools:    []types.ToolSchema{{Name: "web_search"}},
	}, true)
	req.Model = "deepseek-flash"
	d := newRawDump(dir, req)
	d.Line(`data: {"id":"1"}`)
	d.Close()
	d.Line("closed 后写入不得 panic")

	files, _ := filepath.Glob(filepath.Join(dir, "*.sse"))
	if len(files) != 1 {
		t.Fatalf("应生成 1 个转储文件，实际 %v", files)
	}
	b, _ := os.ReadFile(files[0])
	got := string(b)
	for _, want := range []string{`# request {`, `"model":"deepseek-flash"`, `"web_search"`, `"messages":1`, `data: {"id":"1"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("转储缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "机密正文") || strings.Contains(strings.ToLower(got), "authorization") {
		t.Errorf("转储不得含消息正文/鉴权信息:\n%s", got)
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("转储文件权限应为 0600，实际 %v", st.Mode().Perm())
	}
	if newRawDump("", req) != nil {
		t.Error("未配置目录时必须关闭")
	}
}
