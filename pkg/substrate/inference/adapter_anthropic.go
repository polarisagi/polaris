package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// AnthropicAdapter 实现 protocol.Provider，对接 Anthropic Messages API。
type AnthropicAdapter struct {
	model               string
	credentialFn        func() []byte
	client              *http.Client
	caps                protocol.ProviderCapabilities
	enablePromptCaching bool   // 注入 cache_control 标记以激活 prompt caching
	baseURL             string // 空值 → "https://api.anthropic.com"（测试可覆盖）
	tbr                 *observability.TokenBurnRate
}

var _ protocol.Provider = (*AnthropicAdapter)(nil)

// AnthropicOption 适配器选项函数。
type AnthropicOption func(*AnthropicAdapter)

// WithAnthropicPromptCaching 开启 Anthropic prompt caching。
// 向 system prompt 和最后一个 tool 注入 cache_control:{type:"ephemeral"}，
// 命中缓存时 cache_read_input_tokens 费率约为正常输入的 1/10。
func WithAnthropicPromptCaching() AnthropicOption {
	return func(a *AnthropicAdapter) {
		a.enablePromptCaching = true
		a.caps.CostPer1KCacheHit = 0.30 // Anthropic cache read: $0.30/1M tokens
	}
}

// NewAnthropicAdapter 构造 Anthropic 适配器。
func NewAnthropicAdapter(model string, credFn func() []byte, client *http.Client, tbr *observability.TokenBurnRate, opts ...AnthropicOption) *AnthropicAdapter {
	if client == nil {
		client = defaultHTTPClient
	}

	// Wrap transport for key injection
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	customClient := *client // shallow copy
	customClient.Transport = keyInjectRT{inner: transport, keyFn: credFn}

	a := &AnthropicAdapter{
		model:        model,
		credentialFn: credFn,
		client:       &customClient,
		tbr:          tbr,
		caps: protocol.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsThinking:  true,
			SupportsVision:    true, // Claude 3+ 全系支持图像输入
			MaxContextTokens:  200000,
			CostPer1KInput:    3.0,
			CostPer1KOutput:   15.0,
		},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// messagesURL 返回 Messages API 端点（测试可通过 baseURL 覆盖）。
func (a *AnthropicAdapter) messagesURL() string {
	base := a.baseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return base + "/v1/messages"
}

func (a *AnthropicAdapter) ModelID() string {
	return a.model
}

func (a *AnthropicAdapter) Capabilities() protocol.ProviderCapabilities {
	return a.caps
}

func (a *AnthropicAdapter) Tokenizer() protocol.TokenizerAdapter {
	return &simpleTokenizer{}
}

func (a *AnthropicAdapter) Infer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (*protocol.ProviderResponse, error) {
	options := &protocol.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &protocol.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}
	body, err := a.buildAnthropicRequest(req, false)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.messagesURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if req.ThinkingMode != "" && req.ThinkingMode != protocol.ThinkingDisabled {
		httpReq.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
	}

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(httpResp.Body)
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("anthropic: HTTP %d: %s", httpResp.StatusCode, raw))
	}

	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Model      string `json:"model"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "anthropic: decode", err)
	}

	textBuilder := new(strings.Builder)
	var toolCalls []protocol.InferToolCall
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			textBuilder.WriteString(c.Text)
		case "tool_use":
			input := []byte(c.Input)
			if len(input) == 0 {
				input = []byte("{}")
			}
			toolCalls = append(toolCalls, protocol.InferToolCall{
				ID:    c.ID,
				Name:  c.Name,
				Input: input,
			})
		}
	}
	resp := &protocol.ProviderResponse{
		Content:      textBuilder.String(),
		ToolCalls:    toolCalls,
		FinishReason: out.StopReason,
		Model:        out.Model,
		Usage: protocol.Usage{
			InputTokens:         out.Usage.InputTokens,
			OutputTokens:        out.Usage.OutputTokens,
			CacheHitTokens:      out.Usage.CacheReadInputTokens,
			CacheCreationTokens: out.Usage.CacheCreationInputTokens,
		},
	}

	// 扣除 token（EMA + 总量计算）
	if a.tbr != nil {
		a.tbr.Add(int64(resp.Usage.InputTokens + resp.Usage.OutputTokens))
	}

	return resp, nil
}

func (a *AnthropicAdapter) StreamInfer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (<-chan protocol.StreamEvent, error) {
	options := &protocol.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &protocol.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}
	body, err := a.buildAnthropicRequest(req, true)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.messagesURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if req.ThinkingMode != "" && req.ThinkingMode != protocol.ThinkingDisabled {
		httpReq.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
	}

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("anthropic: HTTP %d: %s", httpResp.StatusCode, raw))
	}

	ch := make(chan protocol.StreamEvent, 64)
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()
		a.parseAnthropicStream(ctx, httpResp.Body, ch)
	}()
	return ch, nil
}

func (a *AnthropicAdapter) buildAnthropicRequest(req *protocol.InferRequest, stream bool) ([]byte, error) { //nolint:gocyclo
	model := resolveAnthropicModel(a.model)
	if req.Model != "" {
		model = resolveAnthropicModel(req.Model)
	}

	// 转换 messages
	var msgs []map[string]any
	var system string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system += m.Content + "\n"
			continue
		}
		if len(m.Parts) > 0 {
			var contentBlocks []any
			for _, p := range m.Parts {
				switch v := p.(type) {
				case protocol.ImagePart:
					contentBlocks = append(contentBlocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": v.MediaType,
							"data":       base64.StdEncoding.EncodeToString(v.Data),
						},
					})
				default:
					contentBlocks = append(contentBlocks, v)
				}
			}
			msgs = append(msgs, map[string]any{"role": m.Role, "content": contentBlocks})
		} else {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
		}
	}

	payload := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": req.MaxTokens,
	}
	if system != "" {
		payload["system"] = strings.TrimSpace(system)
	}
	if req.MaxTokens <= 0 {
		payload["max_tokens"] = 4096
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if stream {
		payload["stream"] = true
	}

	if req.ThinkingMode != "" && req.ThinkingMode != protocol.ThinkingDisabled {
		budget := req.ThinkingBudget
		if budget <= 0 {
			budget = 8000
		}
		payload["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": budget,
		}
	}

	// 传入工具 schema（Anthropic tools 格式）
	if len(req.Tools) > 0 {
		anthropicTools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			anthropicTools = append(anthropicTools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": schema,
			})
		}
		payload["tools"] = anthropicTools
	}

	// Anthropic Prompt Caching：system_and_3 策略，最多 4 个断点。
	// 断点 1: system prompt（跨会话稳定，命中率最高）
	// 断点 2: tools 最后一项（工具列表会话内不变）
	// 断点 3+4: 最近 2 条非 system 消息（缓存会话历史前缀，多轮对话收益显著）
	if a.enablePromptCaching { //nolint:nestif
		cacheMarker := map[string]string{"type": "ephemeral"}

		// 断点 1 — system → text array + cache_control
		if system != "" {
			payload["system"] = []map[string]any{
				{"type": "text", "text": strings.TrimSpace(system), "cache_control": cacheMarker},
			}
		}
		// 断点 2 — tools 最后一项
		if tools, ok := payload["tools"].([]map[string]any); ok && len(tools) > 0 {
			tools[len(tools)-1]["cache_control"] = cacheMarker
		}
		// 断点 3+4 — 最近 2 条非 system 消息（按序收集非 system 下标，取末尾 2 条）
		var nonSysIdx []int
		for i, m := range msgs {
			if m["role"] != "system" {
				nonSysIdx = append(nonSysIdx, i)
			}
		}
		start := len(nonSysIdx) - 2
		if start < 0 {
			start = 0
		}
		for _, idx := range nonSysIdx[start:] {
			applyMsgCacheControl(msgs[idx], cacheMarker)
		}
	}

	return json.Marshal(payload)
}

// parseAnthropicStream 解析 Anthropic SSE 事件并转换为统一的 StreamEvent。
// tool_use 事件打包为 StreamToolCall，Content 为 JSON: {"id","name","input"}。
func (a *AnthropicAdapter) parseAnthropicStream(ctx context.Context, body io.Reader, ch chan<- protocol.StreamEvent) { //nolint:gocyclo
	scanner := bufio.NewScanner(body)
	var toolID, toolName string
	var toolInputBuf strings.Builder
	inToolBlock := false
	inThinkingBlock := false // 标记当前是否在处理 extended thinking block

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"` // extended thinking delta
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue
		}

		switch frame.Type {
		case "message_start":
			if frame.Message.Usage.InputTokens > 0 {
				ch <- protocol.StreamEvent{
					Type: protocol.StreamTextDelta,
					Usage: protocol.Usage{
						InputTokens:         frame.Message.Usage.InputTokens,
						CacheHitTokens:      frame.Message.Usage.CacheReadInputTokens,
						CacheCreationTokens: frame.Message.Usage.CacheCreationInputTokens,
					},
				}
				if frame.Message.Usage.InputTokens > 0 {
					if a.tbr != nil {
						a.tbr.Add(int64(frame.Message.Usage.InputTokens))
					}
				}
			}
		case "content_block_start":
			switch frame.ContentBlock.Type {
			case "tool_use":
				toolID = frame.ContentBlock.ID
				toolName = frame.ContentBlock.Name
				toolInputBuf.Reset()
				inToolBlock = true
				inThinkingBlock = false
			case "thinking":
				// Anthropic extended thinking block 开始
				inThinkingBlock = true
				inToolBlock = false
			default:
				inToolBlock = false
				inThinkingBlock = false
			}
		case "content_block_delta":
			switch {
			case inToolBlock && frame.Delta.Type == "input_json_delta":
				toolInputBuf.WriteString(frame.Delta.PartialJSON)
			case inThinkingBlock && frame.Delta.Type == "thinking_delta" && frame.Delta.Thinking != "":
				// 将思考内容发送给前端显示
				ch <- protocol.StreamEvent{Type: protocol.StreamThinking, Content: frame.Delta.Thinking}
			case !inToolBlock && !inThinkingBlock && frame.Delta.Type == "text_delta" && frame.Delta.Text != "":
				ch <- protocol.StreamEvent{Type: protocol.StreamTextDelta, Content: frame.Delta.Text}
			}
		case "content_block_stop":
			if inToolBlock {
				inputJSON := toolInputBuf.String()
				if inputJSON == "" {
					inputJSON = "{}"
				}
				payload, _ := json.Marshal(map[string]any{
					"id":    toolID,
					"name":  toolName,
					"input": json.RawMessage(inputJSON),
				})
				ch <- protocol.StreamEvent{Type: protocol.StreamToolCall, Content: string(payload)}
				inToolBlock = false
			}
			inThinkingBlock = false
		case "message_delta":
			if frame.Usage.OutputTokens > 0 {
				ch <- protocol.StreamEvent{
					Type:  protocol.StreamTextDelta,
					Usage: protocol.Usage{OutputTokens: frame.Usage.OutputTokens},
				}
				if a.tbr != nil {
					a.tbr.Add(int64(frame.Usage.OutputTokens))
				}
			}
		case "message_stop":
			return
		}
	}
}

// applyMsgCacheControl 向单条消息的最后一个 content block 注入 cache_control。
// content 为 string 时转换为 text block 数组；为 []any 时直接在末尾元素追加。
func applyMsgCacheControl(msg map[string]any, marker map[string]string) {
	content := msg["content"]
	switch v := content.(type) {
	case string:
		msg["content"] = []map[string]any{
			{"type": "text", "text": v, "cache_control": marker},
		}
	case []any:
		if len(v) > 0 {
			if last, ok := v[len(v)-1].(map[string]any); ok {
				last["cache_control"] = marker
			}
		}
	}
}

func resolveAnthropicModel(requested string) string {
	switch requested {
	case "claude-instant-1.2", "claude-2.0", "claude-2.1":
		return "claude-3-5-haiku-latest"
	case "claude-3-opus-20240229":
		return "claude-3-5-sonnet-latest"
	default:
		if requested == "" {
			return "claude-3-5-sonnet-latest"
		}
		return requested
	}
}

// keyInjectRT injects the API key safely during the HTTP round trip.
type keyInjectRT struct {
	inner http.RoundTripper
	keyFn func() []byte
}

func (rt keyInjectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	apiKey := rt.keyFn()
	defer clearBytes(apiKey)
	req.Header.Set("x-api-key", string(apiKey))
	resp, err := rt.inner.RoundTrip(req)
	req.Header.Del("x-api-key") // clear the copy from map immediately
	return resp, err
}
