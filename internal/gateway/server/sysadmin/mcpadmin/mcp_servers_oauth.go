package mcpadmin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// MCP OAuth 网关接线（8e-2，basic_authorization.md）：授权入口 / 预注册配置 / 注销。
// 授权服务器回调页与 Client ID Metadata Document 不在本包——它们不带 Polaris
// 令牌，走鉴权豁免精确白名单，注册在顶层 internal/gateway/server（见该包
// oauth_mcp_handlers.go + middleware_auth.go oauthPublicGetPathSet）。
// ============================================================================

// HandleAuthorizeMCPServer 发起一次 MCP OAuth 授权流程。
// POST /v1/mcp-servers/{serverID}/oauth/authorize
// 响应：200 {"authorization_url": "..."}
//
// redirect 基址推导（httputil.ResolveRedirectBase）：请求带 Origin 头时取 Origin
// （鉴权中间件已保证 Cookie 请求同源）；否则取 scheme://Host。stdio 服务器与未找到
// 均由 BeginAuthorization 内部判定，经 apperr.HTTPStatus 统一映射（stdio → 400，
// not found → 404）。
func (h *MCPAdmin) HandleAuthorizeMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	if h.MCPMgr == nil {
		err := apperr.New(apperr.CodeUnimplemented, "mcp oauth: mcp manager not initialized")
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	redirectBase := httputil.ResolveRedirectBase(r)
	authURL, err := h.MCPMgr.BeginAuthorization(r.Context(), id, redirectBase)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, map[string]string{"authorization_url": authURL})
}

// mcpOAuthPreregRequest PUT .../oauth 请求体。ClientSecret 的"保留/清除"语义靠
// presence map 区分（见 HandlePutMCPServerOAuth），本结构体只承载值本身。
type mcpOAuthPreregRequest struct {
	ClientID              string   `json:"client_id"`
	ClientSecret          string   `json:"client_secret"`
	AuthServerMetadataURL string   `json:"auth_server_metadata_url"`
	Scopes                []string `json:"scopes"`
}

// HandlePutMCPServerOAuth 设置预注册 OAuth 客户端配置。
// PUT /v1/mcp-servers/{serverID}/oauth
// body: {"client_id","client_secret","auth_server_metadata_url","scopes":[...]}
//
// client_secret 语义（三态，靠 JSON key 是否出现 + 值是否为 null 区分，标准
// encoding/json 无法用单个 *string 字段区分"未出现"与"出现且为 null"，故额外
// 解析一次 presence map）：
//   - 未出现 或 空字符串 ""：保留原值（不改写 client_secret_enc）。
//   - 显式 null：清除（client_secret_enc 置空）。
//   - 非空字符串：经 Cipher 加密后写入 client_secret_enc。
//
// 响应不回传明文/密文 secret，只回 has_client_secret。
func (h *MCPAdmin) HandlePutMCPServerOAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	if h.ExtRepo == nil {
		http.Error(w, "extension repo not initialized", http.StatusServiceUnavailable)
		return
	}
	row, err := h.ExtRepo.GetMCPServer(r.Context(), id)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if row == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.RespondError(w, "", apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: read body", err), http.StatusBadRequest)
		return
	}
	var req mcpOAuthPreregRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httputil.RespondError(w, "", apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: decode body", err), http.StatusBadRequest)
		return
	}
	if req.AuthServerMetadataURL != "" && !strings.HasPrefix(req.AuthServerMetadataURL, "https://") {
		err := apperr.New(apperr.CodeInvalidInput, "mcp oauth: auth_server_metadata_url must use https")
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	var presence map[string]json.RawMessage
	if err := json.Unmarshal(body, &presence); err != nil {
		httputil.RespondError(w, "", apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: decode body", err), http.StatusBadRequest)
		return
	}

	secretEnc, err := h.resolveClientSecretEnc(row.OAuth, presence, req.ClientSecret)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}

	newCfg := &mcp.RowOAuthConfig{
		ClientID:              req.ClientID,
		ClientSecretEnc:       secretEnc,
		AuthServerMetadataURL: req.AuthServerMetadataURL,
		Scopes:                req.Scopes,
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := h.ExtRepo.UpdateMCPServer(r.Context(), id, map[string]any{
		"oauth":      mcp.MarshalRowOAuthConfig(newCfg),
		"updated_at": now,
	}); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, map[string]any{
		"server_id":                id,
		"client_id":                req.ClientID,
		"auth_server_metadata_url": req.AuthServerMetadataURL,
		"scopes":                   req.Scopes,
		"has_client_secret":        secretEnc != "",
	})
}

// resolveClientSecretEnc 按 client_secret 的三态语义计算最终应落库的密文。
func (h *MCPAdmin) resolveClientSecretEnc(existingOAuthRaw string, presence map[string]json.RawMessage, newSecret string) (string, error) {
	existingCfg, err := mcp.ParseRowOAuthConfig(existingOAuthRaw)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "mcp oauth: parse existing oauth config", err)
	}
	secretEnc := ""
	if existingCfg != nil {
		secretEnc = existingCfg.ClientSecretEnc
	}
	raw, ok := presence["client_secret"]
	if !ok {
		return secretEnc, nil // 未出现：保留原值
	}
	if string(raw) == "null" {
		return "", nil // 显式 null：清除
	}
	if newSecret == "" {
		return secretEnc, nil // 出现但为空字符串：保留原值
	}
	if h.Cipher == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured, refusing to store client secret in plaintext")
	}
	enc, err := h.Cipher.Encrypt(newSecret)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: encrypt client secret", err)
	}
	return enc, nil
}

// HandleDeleteMCPServerOAuthToken 删除该服务器已获取的 OAuth 令牌并重连（"注销"）。
// DELETE /v1/mcp-servers/{serverID}/oauth/token
//
// 重连会因令牌已删除而在下一次连接尝试中收到 401 挑战，进而把服务器重新置为
// AuthRequired（basic_authorization.md §Error Handling，internal/extension/mcp
// oauth_flow.go Add() storeFailed 分支）——这是"注销"在运行时状态上的落地方式，
// 而不是留一个既非已连接又非已注销的中间态。重连失败（如服务器本不需要 OAuth）
// 不影响本次注销请求本身的成功响应。
func (h *MCPAdmin) HandleDeleteMCPServerOAuthToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	if h.ExtRepo == nil {
		http.Error(w, "extension repo not initialized", http.StatusServiceUnavailable)
		return
	}
	row, err := h.ExtRepo.GetMCPServer(r.Context(), id)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if row == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := h.ExtRepo.DeleteMCPOAuthToken(r.Context(), id); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	if h.MCPMgr != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := h.MCPMgr.StartFromDB(ctx, id); err != nil {
			slog.Warn("mcp oauth: reconnect after token revocation", "server_id", id, "err", err)
		}
	}
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
	httputil.WriteJSON(w, map[string]string{"status": "ok", "server_id": id})
}
