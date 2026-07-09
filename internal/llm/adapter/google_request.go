package adapter

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// GoogleAgentPlatformAdapter 请求构建 + SSE 流解析（R7 拆分自 google.go）。
// 结构体/构造/Infer/StreamInfer 见 google.go。
// ============================================================================

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

// buildGeminiRequest 将 InferRequest 转换为 Gemini 原生 JSON 格式。
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

	b, err := json.Marshal(p)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "buildGeminiRequest: marshal payload", err)
	}
	return b, nil
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
