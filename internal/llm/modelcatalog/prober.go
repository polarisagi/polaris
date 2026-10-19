package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// DefaultMaxResponseBodyBytes 探测响应体大小上限（8MB，R2-09）。
const DefaultMaxResponseBodyBytes = 8 * 1024 * 1024

// ProbeProviderModels 按厂商类型探测并返回其实时模型清单（ADR-0115 WP3）。
func ProbeProviderModels(ctx context.Context, client network.SafeHTTPClient, p ProviderInfo) ([]DiscoveredModel, error) {
	if !client.IsSafe() || client.Client == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "modelcatalog: SafeHTTPClient is required")
	}

	switch p.Type {
	case "openai_compat":
		return probeOpenAICompat(ctx, client, p)
	case "anthropic":
		return probeAnthropic(ctx, client, p)
	case "google_agent_platform":
		return probeGoogle(ctx, client, p)
	case "ollama":
		return probeOllama(ctx, client, p)
	default:
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("unsupported provider type for model catalog: %s", p.Type))
	}
}

// ── OpenAI Compatible ─────────────────────────────────────────────────────────

type openAIModelItem struct {
	ID      string `json:"id"`
	Created int64  `json:"created,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

type openAIModelsResp struct {
	Data []openAIModelItem `json:"data"`
}

func probeOpenAICompat(ctx context.Context, client network.SafeHTTPClient, p ProviderInfo) ([]DiscoveredModel, error) {
	baseURL := strings.TrimRight(p.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	// 两种路径探测：有的 base_url 已含 /v1，有的不含。
	var endpoints []string
	if strings.HasSuffix(baseURL, "/v1") {
		endpoints = []string{baseURL + "/models", strings.TrimSuffix(baseURL, "/v1") + "/models"}
	} else {
		endpoints = []string{baseURL + "/v1/models", baseURL + "/models"}
	}

	var rawBody []byte
	var lastErr error
	var successURL string

	for _, ep := range endpoints {
		req, err := http.NewRequestWithContext(ctx, "GET", ep, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, rErr := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxResponseBodyBytes))
		_ = resp.Body.Close()
		if rErr != nil {
			lastErr = rErr
			continue
		}
		if resp.StatusCode == http.StatusOK {
			rawBody = b
			successURL = ep
			break
		}
		lastErr = apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("HTTP %d from %s: %s", resp.StatusCode, ep, string(b)))
	}

	if rawBody == nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeOpenAICompat failed on all endpoints", lastErr)
	}

	var respObj openAIModelsResp
	if err := json.Unmarshal(rawBody, &respObj); err != nil {
		// 备用尝试：有些简易实现直接返回数组
		var list []openAIModelItem
		if jErr := json.Unmarshal(rawBody, &list); jErr == nil {
			respObj.Data = list
		} else {
			return nil, apperr.Wrap(apperr.CodeInternal, "probeOpenAICompat unmarshal json", err)
		}
	}

	_ = successURL
	var out []DiscoveredModel
	for _, item := range respObj.Data {
		mID := strings.TrimSpace(item.ID)
		if mID == "" {
			continue
		}
		itemRaw, _ := json.Marshal(item)
		mod := DiscoveredModel{
			ProviderID:  p.ID,
			ModelID:     mID,
			DisplayName: mID,
			RawJSON:     string(itemRaw),
		}
		inferCapabilitiesFromID(&mod)
		out = append(out, mod)
	}
	return out, nil
}

// ── Anthropic ─────────────────────────────────────────────────────────────────

type anthropicModelItem struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type anthropicModelsResp struct {
	Data    []anthropicModelItem `json:"data"`
	HasMore bool                 `json:"has_more"`
	LastID  string               `json:"last_id"`
}

func probeAnthropic(ctx context.Context, client network.SafeHTTPClient, p ProviderInfo) ([]DiscoveredModel, error) {
	baseURL := strings.TrimRight(p.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	var allItems []anthropicModelItem
	afterID := ""
	const maxAnthropicPages = 50
	pageCount := 0

	for {
		pageCount++
		if pageCount > maxAnthropicPages {
			break
		}
		page, err := fetchAnthropicPage(ctx, client, baseURL, afterID, p.APIKey)
		if err != nil {
			return nil, err
		}
		allItems = append(allItems, page.Data...)
		if !page.HasMore || page.LastID == "" || len(page.Data) == 0 {
			break
		}
		afterID = page.LastID
	}

	return convertAnthropicItems(allItems, p.ID), nil
}

func fetchAnthropicPage(ctx context.Context, client network.SafeHTTPClient, baseURL, afterID, apiKey string) (*anthropicModelsResp, error) {
	reqURL := baseURL + "/v1/models"
	if afterID != "" {
		reqURL += "?after_id=" + url.QueryEscape(afterID)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeAnthropic build request", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeAnthropic request failed", err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxResponseBodyBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeAnthropic read response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("probeAnthropic HTTP %d: %s", resp.StatusCode, string(b)))
	}

	var page anthropicModelsResp
	if err := json.Unmarshal(b, &page); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeAnthropic unmarshal json", err)
	}
	return &page, nil
}

func convertAnthropicItems(items []anthropicModelItem, providerID string) []DiscoveredModel {
	trueVal := true
	var out []DiscoveredModel
	for _, item := range items {
		mID := strings.TrimSpace(item.ID)
		if mID == "" {
			continue
		}
		itemRaw, _ := json.Marshal(item)
		disp := item.DisplayName
		if disp == "" {
			disp = mID
		}
		mod := DiscoveredModel{
			ProviderID:    providerID,
			ModelID:       mID,
			DisplayName:   disp,
			ContextWindow: 200000,
			SupportsTools: &trueVal,
			RawJSON:       string(itemRaw),
		}
		if strings.Contains(mID, "claude-3") {
			mod.SupportsVision = &trueVal
		}
		if strings.Contains(mID, "3-7") || strings.Contains(mID, "thinking") {
			mod.SupportsThinking = &trueVal
		}
		out = append(out, mod)
	}
	return out
}
