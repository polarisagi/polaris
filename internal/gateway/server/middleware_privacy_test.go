package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type multiHeaderWriter struct {
	header   http.Header
	statuses []int
}

func (w *multiHeaderWriter) Header() http.Header { return w.header }
func (w *multiHeaderWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}
func (w *multiHeaderWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestLoggingResponseWriterAllowsInformationalHeaders(t *testing.T) {
	underlying := &multiHeaderWriter{header: make(http.Header)}
	w := NewLoggingResponseWriter(underlying)
	w.WriteHeader(http.StatusEarlyHints)
	w.WriteHeader(http.StatusContinue)
	w.WriteHeader(http.StatusOK)
	w.WriteHeader(http.StatusInternalServerError)

	if len(underlying.statuses) != 3 || underlying.statuses[0] != http.StatusEarlyHints ||
		underlying.statuses[1] != http.StatusContinue || underlying.statuses[2] != http.StatusOK {
		t.Fatalf("expected multiple informational headers followed by one final header, got %v", underlying.statuses)
	}
	if w.statusCode != http.StatusOK || !w.wroteHeader {
		t.Fatalf("final response state: status=%d wroteHeader=%t", w.statusCode, w.wroteHeader)
	}
}

func TestMiddlewareErrorLogOmitsResponseBody(t *testing.T) {
	old := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{rootCtx: ctx}
	s.SetLocalToken("middleware-test-token")
	s.isReady.Store(true)
	req := httptest.NewRequest(http.MethodGet, "/v1/private-error", nil)
	req.Header.Set("X-API-Key", "middleware-test-token")
	w := httptest.NewRecorder()

	s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private-body-sentinel", http.StatusInternalServerError)
	})).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
	if strings.Contains(logs.String(), "private-body-sentinel") {
		t.Fatalf("response body leaked into logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "trace_id=req_") {
		t.Fatalf("trace id missing from error metadata: %s", logs.String())
	}
}

func TestMiddlewarePanicLogRedactsValueAndIncludesStack(t *testing.T) {
	old := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{rootCtx: ctx}
	s.SetLocalToken("middleware-test-token")
	s.isReady.Store(true)
	req := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	req.Header.Set("X-API-Key", "middleware-test-token")
	w := httptest.NewRecorder()

	s.withMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private-panic-sentinel")
	})).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
	if strings.Contains(logs.String(), "private-panic-sentinel") {
		t.Fatalf("panic value leaked into logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "stack=") || !strings.Contains(logs.String(), "middleware_privacy_test.go") {
		t.Fatalf("panic stack missing from logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "trace_id=req_") {
		t.Fatalf("trace id missing from panic metadata: %s", logs.String())
	}
}

func TestMiddlewareOpenAIPanicUsesErrorEnvelopeBeforeHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{rootCtx: ctx}
	s.SetLocalToken("middleware-test-token")
	s.isReady.Store(true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-API-Key", "middleware-test-token")
	recorder := httptest.NewRecorder()
	s.withMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private-panic-sentinel")
	})).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected panic response headers: status=%d content-type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
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
	if body.Error.Type != "server_error" || body.Error.Message != "Internal server error" || strings.Contains(recorder.Body.String(), "private-panic-sentinel") {
		t.Fatalf("unexpected or unsafe panic envelope: %+v body=%s", body.Error, recorder.Body.String())
	}
}

func TestMiddlewarePanicDoesNotRewriteCommittedOpenAISSE(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{rootCtx: ctx}
	s.SetLocalToken("middleware-test-token")
	s.isReady.Store(true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-API-Key", "middleware-test-token")
	recorder := httptest.NewRecorder()
	s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: sentinel\n\n"))
		panic("private-panic-sentinel")
	})).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("committed SSE response was rewritten: status=%d content-type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	if recorder.Body.String() != "data: sentinel\n\n" || strings.Contains(recorder.Body.String(), `"error"`) || strings.Contains(recorder.Body.String(), "private-panic-sentinel") {
		t.Fatalf("panic appended/replaced committed stream: %q", recorder.Body.String())
	}
}
