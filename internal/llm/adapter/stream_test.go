package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestSSEParser_DeepSeek(t *testing.T) {
	// 模拟 DeepSeek 返回的 SSE 流
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)

		// 写入块 1
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)

		// 写入块 2
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world!\"},\"finish_reason\":\"stop\"}]}\n\n"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)

		// 写入 DONE
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer mockServer.Close()

	client := &OpenAICompatibleClient{
		BaseURL:    mockServer.URL, // 替换以使用 mock
		APIKey:     "test-key",
		HTTPClient: mockServer.Client(),
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
		if ev.Type == types.StreamTextDelta {
			results = append(results, ev.Content)
		} else if ev.Type == types.StreamError {
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
