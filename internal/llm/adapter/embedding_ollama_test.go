package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// ADR-0109 D4：Ollama 没有全局线程环境变量，线程上限必须随每次 /api/embed 请求经
// options.num_thread 下发；未设置时不得携带 options（保持 Ollama 默认行为）。
func TestOllamaEmbedding_NumThreadOption(t *testing.T) {
	cases := []struct {
		name      string
		numThread int
		want      int // 0 = options 不应出现
	}{
		{"unset", 0, 0},
		{"set", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			client := &http.Client{Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"embeddings":[[0.1,0.2]]}`))),
					Header:     make(http.Header),
				}
			})}
			a := NewOllamaEmbeddingAdapter("m", client).WithNumThread(tc.numThread)
			if _, err := a.EmbedBatch(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("EmbedBatch: %v", err)
			}
			opts, has := got["options"].(map[string]any)
			if tc.want == 0 {
				if has {
					t.Fatalf("options must be omitted when num_thread unset, got %v", got["options"])
				}
				return
			}
			if !has || int(opts["num_thread"].(float64)) != tc.want {
				t.Fatalf("options.num_thread = %v, want %d", got["options"], tc.want)
			}
		})
	}
}
