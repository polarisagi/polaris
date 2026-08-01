package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 阶段03 R-04：Ollama StreamInfer 接入 TokenBurnRate 回归测试 ───────────
//
// 复用 embedding_openai_test.go 中已定义的 mockRoundTripperFunc（同包内共享）。

// newOllamaTestAdapter 绕过 NewOllamaAdapter 固定的 localhost baseURL，
// 直接构造指向 mock transport 的 OllamaAdapter（同包内可访问未导出字段）。
func newOllamaTestAdapter(rt http.RoundTripper, tbr *metrics.TokenBurnRate) *OllamaAdapter {
	return &OllamaAdapter{
		model: "test-model",
		client: &OpenAICompatibleClient{
			BaseURL:    "http://mock-ollama",
			HTTPClient: &http.Client{Transport: rt},
		},
		tbr: tbr,
	}
}

// TestOllamaAdapter_StreamInfer_TBRAccumulatesAndForwardsAllEvents 验证：
//  1. 3 个 SSE 事件（末事件带 usage）全部转发给下游；
//  2. tbr 按末事件 usage 正确累加（此前 StreamInfer 完全绕过 TokenBurnRate，
//     累加值恒为 0）。
func TestOllamaAdapter_StreamInfer_TBRAccumulatesAndForwardsAllEvents(t *testing.T) {
	sse := "" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world!\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"

	rt := mockRoundTripperFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(sse)),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}
	})

	tbr := metrics.NewTokenBurnRate()
	a := newOllamaTestAdapter(rt, tbr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := a.StreamInfer(ctx, []types.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("StreamInfer 不应报错: %v", err)
	}

	var events []types.StreamEvent
	for ev := range out {
		events = append(events, ev)
	}

	if len(events) != 3 {
		t.Fatalf("期望下游收到 3 个事件，实际收到 %d: %+v", len(events), events)
	}
	last := events[2]
	if last.Usage.InputTokens != 10 || last.Usage.OutputTokens != 5 {
		t.Errorf("末事件 usage 不符：期望 InputTokens=10 OutputTokens=5，实际 %+v", last.Usage)
	}

	if got := tbr.CumulativeTokens(); got != 15 {
		t.Errorf("tbr 累加值不符：期望 15（10+5），实际 %d——StreamInfer 未正确接入 TokenBurnRate", got)
	}
}

// TestOllamaAdapter_StreamInfer_ConsumerCancelStopsForwardingGoroutine 验证
// 消费方通过 ctx 取消提前放弃读取时，转发协程在 100ms 内退出并关闭下游
// channel——此前若无 ctx.Done() 分支，消费方提前离场会导致该协程永久阻塞
// 在 `raw` 上，造成 goroutine 泄漏。
func TestOllamaAdapter_StreamInfer_ConsumerCancelStopsForwardingGoroutine(t *testing.T) {
	rt := mockRoundTripperFunc(func(req *http.Request) *http.Response {
		pr, pw := io.Pipe()
		go func() {
			// 先写一个事件，之后保持连接不关闭——模拟长连接。真实网络场景下
			// 唯一能解除阻塞中 scanner.Scan() 的方式是底层连接因请求 context
			// 被取消而被传输层中断读取；这里显式模拟该行为。
			_, _ = pw.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"chunk1\"},\"finish_reason\":null}]}\n\n"))
			<-req.Context().Done()
			_ = pw.CloseWithError(req.Context().Err())
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       pr,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}
	})

	tbr := metrics.NewTokenBurnRate()
	a := newOllamaTestAdapter(rt, tbr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := a.StreamInfer(ctx, []types.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("StreamInfer 不应报错: %v", err)
	}

	// 消费第一个事件后即"放弃"（提前 return 的模拟：取消 context，不再关心后续流）。
	<-out
	cancel()

	done := make(chan struct{})
	go func() {
		for range out { //nolint:revive // 排空直至 close，证明转发协程已退出
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("消费方取消 context 后 100ms 内转发协程未退出（out 未关闭），疑似 goroutine 泄漏")
	}
}
