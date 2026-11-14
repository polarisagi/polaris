package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ResolveBoundProfile 每轮重新解析以便插件停用或升级后拒绝旧会话。
func (h *ChatHandler) ResolveBoundProfile(ctx context.Context, sessionID string) (*types.AgentProfileSpec, error) {
	if h.PersistenceService == nil || h.PersistenceService.ChatRepo == nil {
		return nil, apperr.New(apperr.CodeInternal, "session profile repository unavailable")
	}
	row, err := h.PersistenceService.ChatRepo.GetSession(ctx, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "session profile lookup", err)
	}
	if row == nil || row.AgentProfile == "" {
		return nil, nil
	}
	if h.ExpertResolver == nil {
		return nil, apperr.New(apperr.CodeServiceUnavailable, "expert profile resolver unavailable")
	}
	profile, _, err := h.ExpertResolver.ResolveNamedExpert(ctx, row.AgentProfile)
	if err != nil {
		if apperr.IsCode(err, apperr.CodeNotFound) {
			return nil, apperr.New(apperr.CodeConflict, "bound expert is no longer available; create a new session")
		}
		return nil, apperr.Wrap(apperr.CodeOf(err), "bound expert resolution failed", err)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "session profile digest", err)
	}
	digest := sha256.Sum256(encoded)
	if profile.Source != row.AgentProfileSource || hex.EncodeToString(digest[:]) != row.AgentProfileDigest {
		return nil, apperr.New(apperr.CodeConflict, "bound expert changed; create a new session")
	}
	return profile, nil
}

// HandleCreateSession 让专家身份在首轮前持久化，避免刷新或重启后丢失绑定。
func (h *ChatHandler) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	req, err := decodeCreateSessionRequest(r.Body)
	if err != nil {
		httputil.RespondError(w, "invalid session request", err, http.StatusBadRequest)
		return
	}
	if !validCreateSessionRequest(req) {
		httputil.RespondError(w, "invalid session fields", nil, http.StatusBadRequest)
		return
	}
	row, err := h.newSessionBinding(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if h.PersistenceService == nil || h.PersistenceService.ChatRepo == nil {
		httputil.RespondError(w, "session storage unavailable", nil, http.StatusServiceUnavailable)
		return
	}
	if err := h.PersistenceService.ChatRepo.CreateSession(r.Context(), row); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	resp := map[string]any{"session_id": row.ID, "project_id": row.ProjectID, "agent_profile": row.AgentProfile}
	if row.ExpertID != "" {
		resp["expert_plugin_id"] = row.ExpertPluginID
		resp["expert_id"] = row.ExpertID
		resp["expert_type"] = row.ExpertType
		resp["expert_display_name"] = json.RawMessage(row.ExpertDisplayName)
	}
	httpStatus := http.StatusCreated
	httputil.WriteJSONStatus(w, httpStatus, resp)
}

type createSessionRequest struct {
	Title        string `json:"title"`
	ProjectID    string `json:"project_id"`
	AgentProfile string `json:"agent_profile"`
}

func decodeCreateSessionRequest(body io.Reader) (createSessionRequest, error) {
	var req createSessionRequest
	dec := json.NewDecoder(io.LimitReader(body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, apperr.Wrap(apperr.CodeInvalidInput, "create session: decode request", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return req, apperr.New(apperr.CodeInvalidInput, "multiple JSON values")
	}
	req.Title, req.AgentProfile = strings.TrimSpace(req.Title), strings.TrimSpace(req.AgentProfile)
	return req, nil
}

func validCreateSessionRequest(req createSessionRequest) bool {
	return len(req.Title) <= 300 && len(req.ProjectID) <= 128 && len(req.AgentProfile) <= 256
}

func (h *ChatHandler) newSessionBinding(ctx context.Context, req createSessionRequest) (types.ChatSessionRow, error) {
	row := types.ChatSessionRow{ID: session.NewSessionID(), Title: req.Title, ProjectID: req.ProjectID, ExpertDisplayName: "{}"}
	if row.ProjectID == "" {
		row.ProjectID = "default"
	}
	if req.AgentProfile == "" {
		return row, nil
	}
	if h.ExpertResolver == nil {
		return row, apperr.New(apperr.CodeServiceUnavailable, "expert definitions unavailable")
	}
	resolved, expert, err := h.ExpertResolver.ResolveNamedExpert(ctx, req.AgentProfile)
	if err != nil {
		return row, apperr.Wrap(apperr.CodeOf(err), "resolve expert for session", err)
	}
	profileJSON, err := json.Marshal(resolved)
	if err != nil {
		return row, apperr.Wrap(apperr.CodeInternal, "encode bound profile", err)
	}
	nameJSON, err := json.Marshal(expert.DisplayName)
	if err != nil {
		return row, apperr.Wrap(apperr.CodeInternal, "encode expert display name", err)
	}
	digest := sha256.Sum256(profileJSON)
	row.AgentProfile, row.AgentProfileSource, row.AgentProfileDigest = resolved.Name, resolved.Source, hex.EncodeToString(digest[:])
	row.ExpertPluginID, row.ExpertID, row.ExpertType, row.ExpertDisplayName = expert.PluginID, expert.ID, expert.Type, string(nameJSON)
	return row, nil
}
