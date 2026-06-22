package adapter

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// GoogleAgentPlatformAdapter 对接 Google Agent Platform (GEAP / Gemini API)。
// 认证方式: ?key=apiKey 查询参数（同 polaris-gateway google translator）。
// 端点路由:
//   - projectID 非空 → GEAP 企业端点 (aiplatform.googleapis.com)
//   - projectID 为空 → Gemini Developer API (generativelanguage.googleapis.com)
type GoogleAgentPlatformAdapter struct {
	model        string
	projectID    string
	location     string
	credentialFn func() []byte
	client       *http.Client
	caps         types.ProviderCapabilities
	tbr          *metrics.TokenBurnRate
}

var _ protocol.Provider = (*GoogleAgentPlatformAdapter)(nil)

func NewGoogleAgentPlatformAdapter(model, projectID, location string, credFn func() []byte, client *http.Client, tbr *metrics.TokenBurnRate) *GoogleAgentPlatformAdapter {
	if client == nil {
		client = defaultHTTPClient()
	}
	return &GoogleAgentPlatformAdapter{
		model:        model,
		projectID:    projectID,
		location:     location,
		credentialFn: credFn,
		client:       client,
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsVision:    true, // Gemini 全系支持图像输入
			SupportsVideo:     true, // Gemini 1.5+ 支持视频文件输入
			MaxContextTokens:  1000000,
			CostPer1KInput:    0.075,
			CostPer1KOutput:   0.30,
		},
		tbr: tbr,
	}
}

func (a *GoogleAgentPlatformAdapter) ModelID() string                          { return a.model }
func (a *GoogleAgentPlatformAdapter) Capabilities() types.ProviderCapabilities { return a.caps }
func (a *GoogleAgentPlatformAdapter) Tokenizer() protocol.TokenizerAdapter {
	return &llmparent.SimpleTokenizer{}
}

// buildEndpoint 构建 GEAP / Gemini API 端点，逻辑与 polaris-gateway buildGoogleTargetURL 对齐。
// location=="global" → aiplatform.googleapis.com（无前缀），路径保留 global
// location=="us-central1" 等区域 → {loc}-aiplatform.googleapis.com
func (a *GoogleAgentPlatformAdapter) buildEndpoint(stream bool) string {
	method := "generateContent"
	if stream {
		method = "streamGenerateContent"
	}
	model := a.model
	if model == "" {
		model = "gemini-2.0-flash"
	}

	if a.projectID != "" {
		host, loc := geapHostAndLoc(a.location)
		path := fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
			a.projectID, loc, model, method)
		if stream {
			return host + path + "?alt=sse"
		}
		return host + path
	}
	// Gemini Developer API
	base := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:%s", model, method)
	if stream {
		return base + "?alt=sse"
	}
	return base
}

// geapHostAndLoc 将 location 字符串解析为 (HTTP host, path 中使用的 location)。
// "global" 或空值 → ("https://aiplatform.googleapis.com", "global")
// 区域值如 "us-central1" → ("https://us-central1-aiplatform.googleapis.com", "us-central1")
func geapHostAndLoc(location string) (string, string) {
	loc := location
	if loc == "" || loc == "global" {
		return "https://aiplatform.googleapis.com", "global"
	}
	return "https://" + loc + "-aiplatform.googleapis.com", loc
}

func appendKey(endpoint, apiKey string) string {
	if strings.Contains(endpoint, "?") {
		return endpoint + "&key=" + apiKey
	}
	return endpoint + "?key=" + apiKey
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// geminiRequest 将 InferRequest 转换为 Gemini 原生 JSON 格式。
func buildGeminiRequest(req *types.InferRequest) ([]byte, error) { //nolint:gocyclo
	type InlineData struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	type FileData struct {
		MimeType string `json:"mimeType"`
		FileURI  string `json:"fileUri"`
	}
	type Part struct {
		Text             string                  `json:"text,omitempty"`
		InlineData       *InlineData             `json:"inlineData,omitempty"`
		FileData         *FileData               `json:"fileData,omitempty"`
		FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
		FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	}
	type Content struct {
		Role  string `json:"role"`
		Parts []Part `json:"parts"`
	}
	type SysInst struct {
		Parts []Part `json:"parts"`
	}
	type GenCfg struct {
		MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
		Temperature     float64 `json:"temperature,omitempty"`
	}
	type Payload struct {
		Contents          []Content    `json:"contents"`
		SystemInstruction *SysInst     `json:"systemInstruction,omitempty"`
		GenerationConfig  *GenCfg      `json:"generationConfig,omitempty"`
		Tools             []geminiTool `json:"tools,omitempty"`
	}

	var sysText string
	var contents []Content
	for _, m := range req.Messages {
		if m.Role == "system" {
			sysText += m.Content + "\n"
			continue
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}

		var parts []Part
		if len(m.Parts) > 0 { //nolint:nestif
			for _, p := range m.Parts {
				if ip, ok := p.(types.ImagePart); ok {
					parts = append(parts, Part{
						InlineData: &InlineData{
							MimeType: ip.MediaType,
							// Gemini inlineData.data 要求 Base64 编码字符串，不能是原始二进制字节
							Data: base64.StdEncoding.EncodeToString(ip.Data),
						},
					})
					continue
				}
				if vp, ok := p.(types.VideoPart); ok {
					parts = append(parts, Part{
						FileData: &FileData{
							MimeType: vp.MediaType,
							FileURI:  vp.URI,
						},
					})
					continue
				}

				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				switch pm["type"] {
				case "text":
					if text, ok := pm["text"].(string); ok {
						parts = append(parts, Part{Text: text})
					}
				case "tool_use":
					name, _ := pm["name"].(string)
					var args map[string]any
					switch v := pm["input"].(type) {
					case json.RawMessage:
						_ = json.Unmarshal(v, &args)
					case map[string]any:
						args = v
					case string:
						_ = json.Unmarshal([]byte(v), &args)
					}
					if args == nil {
						args = make(map[string]any)
					}
					parts = append(parts, Part{FunctionCall: &geminiFunctionCall{Name: name, Args: args}})
				case "tool_result":
					// Gemini 的 tool_result 角色必须是 function，并且名字必须匹配
					// 我们这里由于是从 polaris 协议转过来，把它放到 role="function" 这个特殊的 message 里
					// 但根据 Gemini 官方要求，User 提供 response
					name, _ := pm["name"].(string)
					if name == "" {
						name = "unknown_tool"
					}
					contentStr, _ := pm["content"].(string)
					respData := map[string]any{}
					if err := json.Unmarshal([]byte(contentStr), &respData); err != nil {
						respData["result"] = contentStr
					}
					parts = append(parts, Part{
						FunctionResponse: &geminiFunctionResponse{
							Name:     name,
							Response: respData,
						},
					})
				}
			}
		} else {
			if m.Content != "" {
				parts = append(parts, Part{Text: m.Content})
			}
		}

		// 修正 Gemini 要求的角色名称（tool_result 在 gemini 里其实不需要变 role=function，而是 role=user 就可以？或者 function，这里统一转）
		// 其实根据 Gemini 文档，functionResponse 应该是在 role="function" 或 user 都可以。Gemini 通常用 user。
		if len(parts) > 0 {
			contents = append(contents, Content{Role: role, Parts: parts})
		}
	}

	p := Payload{Contents: contents}
	if sysText != "" {
		p.SystemInstruction = &SysInst{Parts: []Part{{Text: strings.TrimSpace(sysText)}}}
	}
	cfg := &GenCfg{}
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = req.MaxTokens
	} else {
		cfg.MaxOutputTokens = 4096
	}
	if req.Temperature > 0 {
		cfg.Temperature = req.Temperature
	}
	p.GenerationConfig = cfg

	// Tools
	if len(req.Tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
		p.Tools = []geminiTool{{FunctionDeclarations: decls}}
	}

	return json.Marshal(p)
}
func (a *GoogleAgentPlatformAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &types.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}
	body, err := buildGeminiRequest(req)
	if err != nil {
		return nil, fmt.Errorf("GoogleAgentPlatformAdapter.Infer: %w", err)
	}
	apiKey := a.credentialFn()
	defer llmparent.ClearBytes(apiKey)

	endpoint := appendKey(a.buildEndpoint(false), string(apiKey))
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("GoogleAgentPlatformAdapter.Infer: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("GoogleAgentPlatformAdapter.Infer: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(httpResp.Body)
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("google: HTTP %d: %s", httpResp.StatusCode, raw))
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string              `json:"text"`
					FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "google: decode", err)
	}

	text, finishReason := "", ""

	if len(out.Candidates) > 0 {
		finishReason = out.Candidates[0].FinishReason
		for _, p := range out.Candidates[0].Content.Parts {
			if p.Text != "" {
				text += p.Text
			}

		}
	}

	// Gemini doesn't use standard ToolCall struct in InferResponse yet, wait, polaris protocol uses string / json for stream but for non-stream what does it use?
	// The problem is InferResponse only has Content string. Tool calls in non-stream need to be handled if types.InferResponse supports it.
	// Looking at adapter_anthropic.go, Infer() only returns Content string. Our Tool calls are handled primarily in StreamInfer.
	// Wait, types.InferResponse doesn't have ToolCalls natively?
	// Let's check types.InferResponse definition via a quick look.

	resp := &types.ProviderResponse{
		Content:      text,
		FinishReason: finishReason,
		Model:        a.model,
		Usage: types.Usage{
			InputTokens:    out.UsageMetadata.PromptTokenCount,
			OutputTokens:   out.UsageMetadata.CandidatesTokenCount,
			CacheHitTokens: out.UsageMetadata.CachedContentTokenCount,
		},
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		if a.tbr != nil {
			a.tbr.Add(int64(resp.Usage.InputTokens + resp.Usage.OutputTokens))
		}
	}

	metrics.RecordLLMCacheHit("google", req.Model, resp.Usage.CacheHitTokens > 0)

	return resp, nil
}
func (a *GoogleAgentPlatformAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &types.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}
	body, err := buildGeminiRequest(req)
	if err != nil {
		return nil, fmt.Errorf("GoogleAgentPlatformAdapter.StreamInfer: %w", err)
	}
	apiKey := a.credentialFn()

	endpoint := appendKey(a.buildEndpoint(true), string(apiKey))
	llmparent.ClearBytes(apiKey)

	// 给单次推理加 120s 上限，防止 Google 连接 hang 住永不关闭导致前端卡死
	inferCtx, cancel := context.WithTimeout(ctx, 120*time.Second)

	httpReq, err := http.NewRequestWithContext(inferCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("GoogleAgentPlatformAdapter.StreamInfer: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("GoogleAgentPlatformAdapter.StreamInfer: %w", err)
	}
	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		cancel()
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("google: HTTP %d: %s", httpResp.StatusCode, raw))
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		defer cancel()
		defer close(ch)
		defer httpResp.Body.Close()
		parseGoogleStream(inferCtx, httpResp.Body, ch, a.model, a.tbr)
	}()
	return ch, nil
}

func parseGoogleStream(ctx context.Context, body io.Reader, ch chan<- types.StreamEvent, model string, tbr *metrics.TokenBurnRate) {
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return
		}
		var frame struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string              `json:"text"`
						FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata struct {
				PromptTokenCount        int `json:"promptTokenCount"`
				CandidatesTokenCount    int `json:"candidatesTokenCount"`
				CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue
		}
		for _, c := range frame.Candidates {
			for i, p := range c.Content.Parts {
				if p.Text != "" {
					ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: p.Text}
				}
				if p.FunctionCall != nil {
					argsBytes, _ := json.Marshal(p.FunctionCall.Args)
					payload, _ := json.Marshal(map[string]any{
						"id":    fmt.Sprintf("call_%d", i),
						"name":  p.FunctionCall.Name,
						"input": json.RawMessage(argsBytes),
					})
					ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
				}
			}
		}
		if frame.UsageMetadata.CandidatesTokenCount > 0 || frame.UsageMetadata.PromptTokenCount > 0 {
			ch <- types.StreamEvent{
				Type: types.StreamTextDelta,
				Usage: types.Usage{
					InputTokens:    frame.UsageMetadata.PromptTokenCount,
					OutputTokens:   frame.UsageMetadata.CandidatesTokenCount,
					CacheHitTokens: frame.UsageMetadata.CachedContentTokenCount,
				},
			}
			if tbr != nil {
				tbr.Add(int64(frame.UsageMetadata.PromptTokenCount + frame.UsageMetadata.CandidatesTokenCount))
			}
			metrics.RecordLLMCacheHit("google", model, frame.UsageMetadata.CachedContentTokenCount > 0)
		}
	}
}
