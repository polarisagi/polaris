package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"

	"golang.org/x/time/rate"
)

func TestHandleGetConfigDoesNotExposeReadPathOrCause(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "private-config-sentinel.toml")
	t.Setenv(config.EnvPolarisConfig, secretPath)
	server := &Server{}
	recorder := httptest.NewRecorder()
	server.handleGetConfig(recorder, httptest.NewRequest(http.MethodGet, "/v1/config/server", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", recorder.Code)
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content type: got %q", recorder.Header().Get("Content-Type"))
	}
	if strings.Contains(recorder.Body.String(), secretPath) || strings.Contains(recorder.Body.String(), "no such file") {
		t.Fatalf("config path or filesystem cause leaked: %s", recorder.Body.String())
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != string(apperr.CodeInternal) || body.Message != "Configuration file could not be read" || body.Error != body.Message {
		t.Fatalf("unexpected error response: %+v", body)
	}
}

func TestWriteAppErrUsesSharedStatusMappingAndHidesCause(t *testing.T) {
	const secret = "database-path-sentinel"
	err := apperr.Wrap(apperr.CodeStorageUnavailable, "storage unavailable", errors.New(secret)).WithRetryAfter(19)
	recorder := httptest.NewRecorder()
	writeAppErr(recorder, err)

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "19" {
		t.Fatalf("status/retry-after: got %d/%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("storage cause leaked: %s", recorder.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != string(apperr.CodeStorageUnavailable) {
		t.Fatalf("typed code was not preserved: %+v", body)
	}
}

func TestCheckAuthUsesOpenAIErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		locked    bool
		status    int
		errorType string
	}{
		{name: "authentication", status: http.StatusUnauthorized, errorType: "authentication_error"},
		{name: "rate limit", locked: true, status: http.StatusTooManyRequests, errorType: "rate_limit_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const ip = "203.0.113.17"
			manager := &AuthManager{failures: make(map[string]int), lockedAt: make(map[string]time.Time)}
			if tc.locked {
				manager.lockedAt[ip] = time.Now()
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			recorder := httptest.NewRecorder()
			if _, ok := (&Server{}).checkAuth(recorder, req, ip, "api-key-secret-sentinel", manager); ok {
				t.Fatal("unauthenticated request unexpectedly accepted")
			}
			if recorder.Code != tc.status {
				t.Fatalf("status: got %d, want %d", recorder.Code, tc.status)
			}
			if strings.Contains(recorder.Body.String(), "api-key-secret-sentinel") {
				t.Fatalf("credential leaked: %s", recorder.Body.String())
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("OpenAI error envelope is not JSON: %v; body=%s", err, recorder.Body.String())
			}
			if body.Error.Type != tc.errorType || body.Error.Message == "" {
				t.Fatalf("unexpected OpenAI error envelope: %+v", body.Error)
			}
			if tc.locked && recorder.Header().Get("Retry-After") != "300" {
				t.Fatalf("Retry-After was lost: %q", recorder.Header().Get("Retry-After"))
			}
		})
	}
}

func TestGlobalRateLimitUsesOpenAIErrorEnvelope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{rootCtx: ctx, rateLimiter: rate.NewLimiter(0, 0)}
	s.SetLocalToken("middleware-test-token")
	s.isReady.Store(true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-API-Key", "middleware-test-token")
	recorder := httptest.NewRecorder()
	s.withMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("rate-limited request reached handler")
	})).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("status/retry-after: got %d/%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected OpenAI error envelope: %v; body=%s", err, recorder.Body.String())
	}
	if body.Error.Type != "rate_limit_error" || body.Error.Message == "" {
		t.Fatalf("unexpected rate limit error: %+v", body.Error)
	}
}
