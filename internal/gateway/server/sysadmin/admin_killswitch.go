package sysadmin

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type KillSwitchReq struct {
	Reason string `json:"reason"`
}

func (h *SysAdminHandler) HandleKill(w http.ResponseWriter, r *http.Request) {
	if h.KillSwitch == nil {
		httputil.RespondError(w, "killswitch is not configured", apperr.New(apperr.CodeInternal, "killswitch is not configured"), http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	authCtx := authcontext.FromContext(ctx)
	if authCtx.UserID == "" {
		httputil.RespondError(w, "authentication required", apperr.New(apperr.CodeForbidden, "authentication required"), http.StatusForbidden)
		return
	}
	actor := authCtx.UserID

	var req KillSwitchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", apperr.Wrap(apperr.CodeInvalidInput, "invalid request body", err), http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		httputil.RespondError(w, "reason cannot be empty", apperr.New(apperr.CodeInvalidInput, "reason cannot be empty"), http.StatusBadRequest)
		return
	}

	h.KillSwitch.ManualFullStop(actor, req.Reason)
	// Write audit log
	slog.Info("KillSwitch_ManualFullStop", "actor", actor, "reason", req.Reason)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "sealed"})
}

func (h *SysAdminHandler) HandleUnseal(w http.ResponseWriter, r *http.Request) {
	if h.KillSwitch == nil {
		httputil.RespondError(w, "killswitch is not configured", apperr.New(apperr.CodeInternal, "killswitch is not configured"), http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	authCtx := authcontext.FromContext(ctx)

	// B1: 鉴权加固（unseal 是最高敏感操作，不适用常规宽松规则）
	// unseal 需要有效 POLARIS_API_KEY，不接受匿名/loopback 豁免
	if authCtx.UserID != "admin" {
		httputil.RespondError(w, "unseal requires valid POLARIS_API_KEY", apperr.New(apperr.CodeForbidden, "unseal requires valid POLARIS_API_KEY, anonymous/loopback exemption is not accepted"), http.StatusForbidden)
		return
	}
	actor := authCtx.UserID

	var req KillSwitchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", apperr.Wrap(apperr.CodeInvalidInput, "invalid request body", err), http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		httputil.RespondError(w, "reason cannot be empty", apperr.New(apperr.CodeInvalidInput, "reason cannot be empty"), http.StatusBadRequest)
		return
	}

	if err := h.KillSwitch.Unseal(ctx, actor, req.Reason); err != nil {
		httputil.RespondError(w, "failed to unseal", err, http.StatusInternalServerError)
		return
	}
	// Write audit log
	slog.Info("KillSwitch_Unseal", "actor", actor, "reason", req.Reason)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "unsealed"})
}
