package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ============================================================================
// 厂商连通性探测 + reloadProviders 辅助（R7 拆分自 providers.go）。
// ProviderConfig/ProviderModel 类型定义 + providers CRUD 见 providers.go；
// provider_models CRUD + model-roles 见 providers_models.go。
// ============================================================================

// ── probe ─────────────────────────────────────────────────────────────────────

func probeProvider(ctx context.Context, client *http.Client, typ, baseURL, apiKey, modelID, projectID, location string) (bool, string) { //nolint:gocyclo
	switch typ {
	case "openai_compat", "ollama":
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(baseURL, "/")+"/v1/models", nil)
		if err != nil {
			return false, fmt.Sprintf("请求构建失败: %v", err)
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 {
			return false, "API Key 无效（HTTP 401）"
		}
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)

	case "anthropic":
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/v1/models", nil)
		if err != nil {
			return false, fmt.Sprintf("请求构建失败: %v", err)
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		if resp.StatusCode == 401 {
			return false, "API Key 无效（HTTP 401）"
		}
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)

	case "google_agent_platform":
		if apiKey == "" {
			return false, "缺少 API Key"
		}
		model := modelID
		if model == "" {
			model = "gemini-2.0-flash"
		}
		var endpoint string
		if projectID != "" {
			loc := location
			if loc == "" {
				loc = "global"
			}
			var host string
			if loc == "global" {
				host = "https://aiplatform.googleapis.com"
			} else {
				host = "https://" + loc + "-aiplatform.googleapis.com"
			}
			endpoint = fmt.Sprintf(
				"%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent?key=%s",
				host, projectID, loc, model, apiKey)
		} else {
			endpoint = fmt.Sprintf(
				"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
				model, apiKey)
		}
		reqBody := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"maxOutputTokens":1}}`
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(reqBody))
		if err != nil {
			return false, fmt.Sprintf("请求构建失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		limit := len(raw)
		if limit > 200 {
			limit = 200
		}
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:limit])))
	}
	return false, "未知厂商类型"
}

func (h *ProviderHandler) reloadProviders() {
	if h.Registry == nil || h.DB == nil {
		return
	}
	if h.ReloadProviders != nil {
		h.ReloadProviders()
	}
}
