package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func cliHTTPErrorText(resp *http.Response) string {
	if resp == nil {
		return "HTTP request failed"
	}
	statusText := fmt.Sprintf("HTTP %d", resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return statusText + ": request failed"
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return statusText + ": request failed"
	}

	message, parsedJSON := cliPublicErrorMessage(raw)
	if strings.TrimSpace(message) != "" {
		return statusText + ": " + strings.TrimSpace(message)
	}
	if parsedJSON || strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		return statusText + ": request failed"
	}
	return statusText + ": " + body
}

func cliPublicErrorMessage(raw []byte) (string, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return "", false
	}

	var message string
	if err := json.Unmarshal(payload["message"], &message); err == nil && strings.TrimSpace(message) != "" {
		return message, true
	}
	if err := json.Unmarshal(payload["error"], &message); err == nil && strings.TrimSpace(message) != "" {
		return message, true
	}
	var nested struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload["error"], &nested); err == nil {
		return nested.Message, true
	}
	return "", true
}
