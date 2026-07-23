package adapter

import (
	"context"
	"io"
	"net/http"
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

	ch, err := client.SendStreamRequest(ctx, []byte("test-key"), translateRequest(req, true), 0)
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
