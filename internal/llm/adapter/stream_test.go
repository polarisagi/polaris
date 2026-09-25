package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestSSEParser_DeepSeek(t *testing.T) {
	// 模拟 DeepSeek 返回的 SSE 流
	client := &OpenAICompatibleClient{
		BaseURL: "http://dummy", // 替换以使用 mock
		APIKey:  "test-key",
		HTTPClient: &http.Client{
			Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
				pr, pw := io.Pipe()
				go func() {
					// 写入块 1
					pw.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n"))
					time.Sleep(10 * time.Millisecond)

					// 写入块 2
					pw.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world!\"},\"finish_reason\":\"stop\"}]}\n\n"))
					time.Sleep(10 * time.Millisecond)

					// 写入 DONE
					pw.Write([]byte("data: [DONE]\n\n"))
					pw.Close()
				}()

				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       pr,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				}
			}),
		},
	}

	req := &types.InferRequest{
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := client.SendStreamRequest(ctx, nil, []byte("test-key"), translateRequest(req, true), 0)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var results []string
	for ev := range ch {
		switch ev.Type {
		case types.StreamTextDelta:
			results = append(results, ev.Content)
		case types.StreamError:
			t.Fatalf("stream error: %v", ev.Content)
		}
	}

	if len(results) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(results))
	}

	if results[0] != "Hello " || results[1] != "world!" {
		t.Errorf("unexpected content: %v", results)
	}
}

// TestSSEParser_MalformedBody 复现 2026-09-22 empty_response 排查中发现的另一个
// 同族根因：网关/代理对 base_url 返回 HTTP 200，但 body 不是合法 SSE 帧（例如一段
// HTML 或纯文本错误页）。此前每一行都被 `data, ok := strings.CutPrefix(...)` 的
// !ok 分支静默 continue，scanner.Scan() 最终正常返回 false（真 EOF），函数直接
// 落到 defer close(ch)，ch 里空空如也、没有任何错误——与上游误判为"推理成功但
// 为空"是同一缺陷类。必须能产出 StreamError，不能悄无声息地把 ch 关空。
func TestSSEParser_MalformedBody(t *testing.T) {
	client := &OpenAICompatibleClient{
		BaseURL: "http://dummy",
		APIKey:  "test-key",
		HTTPClient: &http.Client{
			Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
				body := io.NopCloser(strings.NewReader("<html><body>502 Bad Gateway</body></html>\n"))
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       body,
					Header:     http.Header{"Content-Type": []string{"text/html"}},
				}
			}),
		},
	}

	req := &types.InferRequest{
		Messages: []types.Message{{Role: "user", Content: "hi"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := client.SendStreamRequest(ctx, nil, []byte("test-key"), translateRequest(req, true), 0)
	if err != nil {
		t.Fatalf("expected no error at request time, got %v", err)
	}

	var gotErr bool
	for ev := range ch {
		if ev.Type == types.StreamError {
			gotErr = true
		}
		if ev.Type == types.StreamTextDelta && ev.Content != "" {
			t.Fatalf("unexpected content from malformed body: %q", ev.Content)
		}
	}

	if !gotErr {
		t.Fatal("expected StreamError for malformed non-SSE 200 body, got silent empty channel")
	}
}

// TestSSEParser_ToolCallsFlushedWithoutToolCallsFinish 复现 2026-09-25 实测：Provider
// 以非 "tool_calls" 的 finish_reason 收尾时，已聚合的工具调用在 [DONE] 处被静默丢弃，
// 上游只看到"有思考、无正文、无工具调用"。
func TestSSEParser_ToolCallsFlushedWithoutToolCallsFinish(t *testing.T) {
	client := &OpenAICompatibleClient{
		BaseURL: "http://dummy",
		APIKey:  "test-key",
		HTTPClient: &http.Client{
			Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
				body := strings.Join([]string{
					`data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"需要查系统信息"},"finish_reason":null}]}`,
					`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"sys_probe","arguments":"{\"kind\":"}}]},"finish_reason":null}]}`,
					`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"memory\"}"}}]},"finish_reason":"stop"}]}`,
					`data: [DONE]`,
				}, "\n\n") + "\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				}
			}),
		},
	}
	req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "hi"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := client.SendStreamRequest(ctx, nil, []byte("test-key"), translateRequest(req, true), 0)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	var calls []string
	for ev := range ch {
		if ev.Type == types.StreamToolCall {
			calls = append(calls, ev.Content)
		}
	}
	if len(calls) != 1 || !strings.Contains(calls[0], `"sys_probe"`) || !strings.Contains(calls[0], `"memory"`) {
		t.Fatalf("工具调用应在流结束时补发，得到 %v", calls)
	}
}

// TestSSEParser_SparseToolCallIndex 工具调用 index 不从 0 开始时不得被静默跳过。
func TestSSEParser_SparseToolCallIndex(t *testing.T) {
	client := &OpenAICompatibleClient{
		BaseURL: "http://dummy",
		APIKey:  "test-key",
		HTTPClient: &http.Client{
			Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
				body := strings.Join([]string{
					`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_a","function":{"name":"sys_probe","arguments":"{}"}}]},"finish_reason":null}]}`,
					`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"call_b","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
					`data: [DONE]`,
				}, "\n\n") + "\n\n"
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
					Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
			}),
		},
	}
	req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "hi"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := client.SendStreamRequest(ctx, nil, []byte("test-key"), translateRequest(req, true), 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ev := range ch {
		if ev.Type == types.StreamToolCall {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("稀疏 index 的 2 个工具调用应全部发出，实际 %d", n)
	}
}

// TestSSEParser_ToolCallsAtEOFNotReportedAsEmpty 流只含工具调用、既无 finish_reason
// 也无 [DONE] 就 EOF：补发工具调用后不得再追加"no valid SSE data frame"错误。
func TestSSEParser_ToolCallsAtEOFNotReportedAsEmpty(t *testing.T) {
	client := &OpenAICompatibleClient{
		BaseURL: "http://dummy",
		APIKey:  "test-key",
		HTTPClient: &http.Client{
			Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
				body := `data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"sys_probe","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n"
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
					Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
			}),
		},
	}
	req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "hi"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := client.SendStreamRequest(ctx, nil, []byte("test-key"), translateRequest(req, true), 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for ev := range ch {
		switch ev.Type {
		case types.StreamToolCall:
			calls++
		case types.StreamError:
			t.Fatalf("EOF 补发工具调用后不应再报流错误: %s", ev.Content)
		}
	}
	if calls != 1 {
		t.Fatalf("应补发 1 个工具调用，实际 %d", calls)
	}
}
