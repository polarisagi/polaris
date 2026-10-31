package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type codeActEngineStub struct {
	err       error
	available bool
}

func (s codeActEngineStub) ExecuteCode(context.Context, protocol.CodeActRequest) (*protocol.CodeActResult, error) {
	return nil, s.err
}

func (s codeActEngineStub) CodeActAvailable() bool { return s.available }

func TestHandleCodeActErrorsUseSafeStructuredResponses(t *testing.T) {
	const cause = "private-codeact-cause-sentinel"
	tests := []struct {
		name        string
		engine      CodeActEngine
		body        string
		wantCode    apperr.Code
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "engine unavailable",
			engine:      codeActEngineStub{},
			body:        `{}`,
			wantCode:    apperr.CodeServiceUnavailable,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: http.StatusText(http.StatusServiceUnavailable),
		},
		{
			name:        "malformed request",
			engine:      codeActEngineStub{available: true},
			body:        `{`,
			wantCode:    apperr.CodeInvalidInput,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "invalid request body",
		},
		{
			name:        "required fields",
			engine:      codeActEngineStub{available: true},
			body:        `{}`,
			wantCode:    apperr.CodeInvalidInput,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "language, code, capability_id are required",
		},
		{
			name:        "execution failure",
			engine:      codeActEngineStub{available: true, err: apperr.Wrap(apperr.CodeStorageUnavailable, "sandbox unavailable", errors.New(cause))},
			body:        `{"language":"python","code":"print(1)","capability_id":"cap-test"}`,
			wantCode:    apperr.CodeStorageUnavailable,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: http.StatusText(http.StatusServiceUnavailable),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{codeActEngine: tt.engine}
			req := httptest.NewRequest(http.MethodPost, "/v1/agent/codeact", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			s.handleCodeAct(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status: want %d, got %d", tt.wantStatus, w.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
			}
			if body["code"] != string(tt.wantCode) || body["message"] != tt.wantMessage || body["error"] != body["message"] {
				t.Fatalf("unexpected error response: %#v", body)
			}
			if strings.Contains(w.Body.String(), cause) {
				t.Fatalf("execution cause leaked into response: %s", w.Body.String())
			}
		})
	}
}
