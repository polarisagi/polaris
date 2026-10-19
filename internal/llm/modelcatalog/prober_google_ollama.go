package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ── Google Agent Platform ─────────────────────────────────────────────────────

type googleModelItem struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type googleModelsResp struct {
	Models []googleModelItem `json:"models"`
}

func probeGoogle(ctx context.Context, client network.SafeHTTPClient, p ProviderInfo) ([]DiscoveredModel, error) {
	if p.APIKey == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "google_agent_platform requires api_key for models.list")
	}

	reqURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s", p.APIKey)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeGoogle build request", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeGoogle request failed", err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxResponseBodyBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeGoogle read body", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("probeGoogle HTTP %d: %s", resp.StatusCode, string(b)))
	}

	var res googleModelsResp
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeGoogle unmarshal json", err)
	}

	trueVal := true
	var out []DiscoveredModel
	for _, item := range res.Models {
		mID := strings.TrimPrefix(item.Name, "models/")
		if mID == "" {
			continue
		}
		itemRaw, _ := json.Marshal(item)
		disp := item.DisplayName
		if disp == "" {
			disp = mID
		}
		mod := DiscoveredModel{
			ProviderID:    p.ID,
			ModelID:       mID,
			DisplayName:   disp,
			ContextWindow: item.InputTokenLimit,
			MaxOutput:     item.OutputTokenLimit,
			SupportsTools: &trueVal,
			RawJSON:       string(itemRaw),
		}
		if strings.Contains(mID, "gemini") {
			mod.SupportsVision = &trueVal
		}
		if strings.Contains(mID, "thinking") || strings.Contains(mID, "2.0-flash-thinking") {
			mod.SupportsThinking = &trueVal
		}
		out = append(out, mod)
	}
	return out, nil
}

// ── Ollama ────────────────────────────────────────────────────────────────────

type ollamaModelItem struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Details struct {
		Format string `json:"format"`
		Family string `json:"family"`
	} `json:"details"`
}

type ollamaModelsResp struct {
	Models []ollamaModelItem `json:"models"`
}

func probeOllama(ctx context.Context, client network.SafeHTTPClient, p ProviderInfo) ([]DiscoveredModel, error) {
	baseURL := strings.TrimRight(p.BaseURL, "/")
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	reqURL := baseURL + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeOllama build request", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeOllama request failed", err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxResponseBodyBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeOllama read body", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("probeOllama HTTP %d: %s", resp.StatusCode, string(b)))
	}

	var res ollamaModelsResp
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "probeOllama unmarshal json", err)
	}

	trueVal := true
	var out []DiscoveredModel
	for _, item := range res.Models {
		mID := item.Name
		if mID == "" {
			mID = item.Model
		}
		if mID == "" {
			continue
		}
		itemRaw, _ := json.Marshal(item)
		mod := DiscoveredModel{
			ProviderID:    p.ID,
			ModelID:       mID,
			DisplayName:   mID,
			ContextWindow: 8192,
			SupportsTools: &trueVal,
			RawJSON:       string(itemRaw),
		}
		if strings.Contains(mID, "llava") || strings.Contains(mID, "vision") {
			mod.SupportsVision = &trueVal
		}
		if strings.Contains(mID, "r1") || strings.Contains(mID, "reason") {
			mod.SupportsThinking = &trueVal
		}
		out = append(out, mod)
	}
	return out, nil
}

func inferCapabilitiesFromID(mod *DiscoveredModel) {
	trueVal := true
	lower := strings.ToLower(mod.ModelID)

	if hasToolCapability(lower) {
		mod.SupportsTools = &trueVal
	}
	if hasVisionCapability(lower) {
		mod.SupportsVision = &trueVal
	}
	if hasThinkingCapability(lower) {
		mod.SupportsThinking = &trueVal
	}
	if cw := inferContextWindow(lower); cw > 0 {
		mod.ContextWindow = cw
	}
}

func hasToolCapability(lower string) bool {
	for _, sub := range []string{"embed", "whisper", "tts", "dall-e"} {
		if strings.Contains(lower, sub) {
			return false
		}
	}
	return true
}

func hasVisionCapability(lower string) bool {
	for _, sub := range []string{"vision", "4o", "vl", "omni", "gemini", "claude"} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func hasThinkingCapability(lower string) bool {
	for _, sub := range []string{"r1", "reasoner", "o1", "o3", "thinking"} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func inferContextWindow(lower string) int {
	switch {
	case strings.Contains(lower, "gemini"):
		return 1048576
	case strings.Contains(lower, "claude"):
		return 200000
	case strings.Contains(lower, "gpt-4o"), strings.Contains(lower, "o1"), strings.Contains(lower, "o3"), strings.Contains(lower, "deepseek"):
		return 128000
	default:
		return 0
	}
}
