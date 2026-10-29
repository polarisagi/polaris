package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type errHITL struct{ err error }

func (errHITL) Prompt(context.Context, types.HITLPrompt) (*types.HITLResponse, error) {
	return nil, nil
}
func (e errHITL) Respond(context.Context, string, types.HITLResponse) error { return e.err }
func (errHITL) Pending(context.Context) ([]types.HITLPrompt, error)         { return nil, nil }

// 裁决错误必须映射为对应 HTTP 状态：未知 ID 404（此前一律 500）、已决 409、前置校验不过 403。
func TestHandleResolveApproval_StatusFollowsErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown id", apperr.New(apperr.CodeNotFound, "x"), http.StatusNotFound},
		{"already decided", apperr.New(apperr.CodeConflict, "x"), http.StatusConflict},
		{"guard failed", apperr.New(apperr.CodeForbidden, "x"), http.StatusForbidden},
		{"internal", apperr.New(apperr.CodeInternal, "x"), http.StatusInternalServerError},
		{"ok", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{hitlGateway: errHITL{err: tc.err}}
			req := httptest.NewRequest("POST", "/v1/approvals/ap_1/resolve", bytes.NewBufferString(`{"action":"deny"}`))
			w := httptest.NewRecorder()
			s.handleResolveApproval(w, req)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%q)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
