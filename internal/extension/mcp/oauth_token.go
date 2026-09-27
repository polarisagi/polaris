package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CredentialCipher 敏感配置加解密（consumer-side；实现为 security/credential.Vault）。
// 与 internal/extension/lifecycle.CredentialCipher 同形——两处按各自消费方独立定义，
// 符合 Go interface 在调用方定义的惯例（HE-3）。
type CredentialCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// TokenSource 为 MCPClient 提供访问令牌。Token 内部按需刷新（过期前 60 秒或已被
// Invalidate 标记）；Invalidate 使下一次 Token 调用强制尝试刷新，供 401 重试一次使用。
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// dbTokenSource 基于 mcp_oauth_tokens 表的 TokenSource 实现。
type dbTokenSource struct {
	manager      *MCPManager
	serverID     string
	forceRefresh atomic.Bool
}

// newTokenSource 构造基于 mcp_oauth_tokens 的令牌来源。
func (m *MCPManager) newTokenSource(serverID string) TokenSource {
	return &dbTokenSource{manager: m, serverID: serverID}
}

// attachTokenSourceIfNeeded 仅对"确实可能使用 OAuth"的远程连接附加 TokenSource：
// 已有令牌，或行声明了预注册 client_id（意味着这台服务器打算走 OAuth）。不满足条件时
// 不附加——避免对纯 Headers 鉴权 / 无鉴权的服务器每次请求都触发一次空令牌查询和日志。
// STDIO 传输一律跳过（basic_authorization.md：STDIO SHOULD NOT 走本规范）。
func (m *MCPManager) attachTokenSourceIfNeeded(ctx context.Context, client *MCPClient, serverID string, cfg MCPClientConfig) {
	if cfg.Transport == MCPStdio || m.rowRepo == nil {
		return
	}
	if tok, err := m.rowRepo.GetMCPOAuthToken(ctx, serverID); err == nil && tok != nil {
		client.SetTokenSource(m.newTokenSource(serverID))
		return
	}
	row, err := m.rowRepo.GetMCPServer(ctx, serverID)
	if err != nil || row == nil {
		return
	}
	rowCfg, err := parseRowOAuthConfig(row.OAuth)
	if err == nil && rowCfg != nil && rowCfg.ClientID != "" {
		client.SetTokenSource(m.newTokenSource(serverID))
	}
}

func (ts *dbTokenSource) Invalidate() { ts.forceRefresh.Store(true) }

func (ts *dbTokenSource) Token(ctx context.Context) (string, error) {
	m := ts.manager
	if m.rowRepo == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp oauth: repo not configured")
	}
	tok, err := m.rowRepo.GetMCPOAuthToken(ctx, ts.serverID)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: load token", err)
	}
	if tok == nil {
		return "", apperr.New(apperr.CodeUnauthorized, "mcp oauth: no token stored, authorization required")
	}
	force := ts.forceRefresh.Swap(false)
	if !force && !tokenNearExpiry(tok.ExpiresAt) {
		return m.decryptAccessToken(tok)
	}
	return m.refreshSerialized(ctx, ts.serverID, tok.AccessTokenEnc, force)
}

// refreshSerialized 同一服务器的刷新串行化：并发请求同时撞上过期时，若各自刷新，
// 启用了刷新令牌轮换的授权服务器会让第二个及以后的请求拿已作废的 refresh_token 失败，
// 进而把服务器误置为需授权。拿到锁后重读一次：staleEnc 已被别人换掉即直接用新令牌。
func (m *MCPManager) refreshSerialized(ctx context.Context, serverID, staleEnc string, force bool) (string, error) {
	lk, _ := m.oauthRefreshLocks.LoadOrStore(serverID, &sync.Mutex{})
	mu := lk.(*sync.Mutex) //nolint:forcetypeassert // 只存 *sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	tok, err := m.rowRepo.GetMCPOAuthToken(ctx, serverID)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: reload token", err)
	}
	if tok == nil {
		return "", apperr.New(apperr.CodeUnauthorized, "mcp oauth: no token stored, authorization required")
	}
	if tok.AccessTokenEnc != staleEnc && (force || !tokenNearExpiry(tok.ExpiresAt)) {
		return m.decryptAccessToken(tok)
	}
	return m.refreshToken(ctx, serverID, tok)
}

// tokenNearExpiry expires_at 为空表示未知/不过期（不主动刷新）；否则提前 60 秒判定过期。
func tokenNearExpiry(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false // 解析失败不阻断请求，交给服务端 401 兜底
	}
	return time.Now().Add(60 * time.Second).After(t)
}

func (m *MCPManager) decryptAccessToken(tok *types.MCPOAuthTokenRow) (string, error) {
	if m.cipher == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured")
	}
	tokStr, err := m.cipher.Decrypt(tok.AccessTokenEnc)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: decrypt access token", err)
	}
	return tokStr, nil
}

// refreshToken 用 refresh_token 换取新的 access_token；成功则写回存储，失败则把服务器
// 标记为需要授权（AuthRequired）。每次调用只尝试一次刷新，不做内部重试循环。
func (m *MCPManager) refreshToken(ctx context.Context, serverID string, tok *types.MCPOAuthTokenRow) (string, error) {
	scopes := strings.Fields(tok.Scopes)
	if tok.RefreshTokenEnc == "" {
		m.markAuthRequiredScopes(serverID, scopes)
		return "", apperr.New(apperr.CodeUnauthorized, "mcp oauth: access token expired and no refresh token available")
	}
	if m.cipher == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured")
	}
	refreshTok, err := m.cipher.Decrypt(tok.RefreshTokenEnc)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: decrypt refresh token", err)
	}
	tokenEndpoint, err := m.tokenEndpointFor(ctx, serverID, tok.Issuer)
	if err != nil {
		m.markAuthRequiredScopes(serverID, scopes)
		return "", err
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshTok}, "resource": {tok.Resource}}
	if err := m.addClientCredentials(ctx, serverID, tok, form); err != nil {
		return "", err
	}
	resp, err := postForm[tokenResponse](ctx, m.httpClient, tokenEndpoint, form)
	if err != nil {
		slog.Warn("mcp oauth: token refresh failed", "server_id", serverID)
		m.markAuthRequiredScopes(serverID, scopes)
		return "", apperr.Wrap(apperr.CodeUnauthorized, "mcp oauth: token refresh failed", err)
	}
	if err := m.persistRefreshedToken(ctx, tok, resp); err != nil {
		return "", err
	}
	slog.Info("mcp oauth: token refreshed", "server_id", serverID)
	return resp.AccessToken, nil
}

// persistRefreshedToken 把刷新得到的新令牌加密写回 mcp_oauth_tokens；resp.RefreshToken 为空时
// 沿用旧 refresh_token（AS 未旋转刷新令牌是常见行为，OAuth 2.1 §4.3.1 仅要求公开客户端旋转）。
func (m *MCPManager) persistRefreshedToken(ctx context.Context, tok *types.MCPOAuthTokenRow, resp *tokenResponse) error {
	newRow := *tok
	accessEnc, err := m.cipher.Encrypt(resp.AccessToken)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp oauth: encrypt refreshed access token", err)
	}
	newRow.AccessTokenEnc = accessEnc
	if resp.RefreshToken != "" {
		refreshEnc, err := m.cipher.Encrypt(resp.RefreshToken)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "mcp oauth: encrypt refreshed refresh token", err)
		}
		newRow.RefreshTokenEnc = refreshEnc
	}
	if resp.Scope != "" {
		newRow.Scopes = resp.Scope
	}
	newRow.ExpiresAt = ""
	if resp.ExpiresIn > 0 {
		newRow.ExpiresAt = time.Now().UTC().Add(time.Duration(resp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	newRow.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := m.rowRepo.UpsertMCPOAuthToken(ctx, newRow); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp oauth: persist refreshed token", err)
	}
	return nil
}

// tokenEndpointFor 重新发现 token_endpoint（不缓存：刷新频率远低于工具调用，正确性优先）。
func (m *MCPManager) tokenEndpointFor(ctx context.Context, serverID, issuer string) (string, error) {
	if m.rowRepo == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp oauth: repo not configured")
	}
	row, err := m.rowRepo.GetMCPServer(ctx, serverID)
	if err != nil || row == nil {
		return "", apperr.New(apperr.CodeNotFound, "mcp oauth: server not found for refresh: "+serverID)
	}
	allowHTTP := false
	if u, perr := url.Parse(row.URL); perr == nil {
		allowHTTP = isLoopbackURL(u)
	}
	asMeta, err := discoverAuthServerMetadata(ctx, m.httpClient, issuer, allowHTTP)
	if err != nil {
		return "", err
	}
	return asMeta.TokenEndpoint, nil
}

// applyTokenSourceHeader 用 TokenSource 覆盖 Authorization 头（basic_authorization.md：不得
// 向 MCP 服务器发送非其 AS 签发的令牌，因此配置头里静态声明的 Authorization 一律被覆盖）。
// nil TokenSource（未配置 OAuth 的服务器）不做任何事，保留原有 cfg.Headers 行为。
func (c *MCPClient) applyTokenSourceHeader(ctx context.Context, req *http.Request) {
	ts := c.tokenSource.Load()
	if ts == nil {
		return
	}
	tok, err := (*ts).Token(ctx)
	if err != nil {
		// 不打印令牌本身之外的任何数据；请求随后会被服务端 401 拒绝，由上层统一处理。
		slog.Debug("mcp: token source unavailable for request", "server", c.cfg.ServerName)
		return
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// markAuthRequiredScopes 把服务器标记为需要（重新）授权，供 ListServers 的 AuthRequired/
// AuthScopes 字段与后续 BeginAuthorization 的 step-up scope 并集使用。
func (m *MCPManager) markAuthRequiredScopes(serverID string, scopes []string) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	e.setAuthRequired(scopes, "")
	slog.Warn("mcp oauth: server marked as requiring authorization", "server_id", serverID)
}

// authRequiredError 检测 err 链中的 OAuth 挑战（401 刷新已失败，或 403 insufficient_scope），
// 置位 AuthRequired 并返回面向 LLM/调用方的可读错误；非 OAuth 挑战返回 nil。
func (m *MCPManager) authRequiredError(serverID, serverName string, err error) error {
	ace := extractAuthChallenge(err)
	if ace == nil {
		return nil
	}
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	scopes := strings.Fields(ace.Scope)
	if ok {
		e.setAuthRequired(scopes, ace.ResourceMetadata)
	}
	code := apperr.CodeUnauthorized
	if ace.Status == http.StatusForbidden {
		code = apperr.CodeForbidden
	}
	return apperr.New(code, "connector "+serverName+" requires authorization")
}

// addClientCredentials 刷新请求携带签发令牌的客户端身份（OAuth 2.1 §4.3.1：公开客户端必须带
// client_id；机密客户端带 client_secret，client_secret_post 与授权码交换一致）。密钥来源：
// 行内预注册 client_id 与令牌一致时取行配置，否则按 (issuer, redirect_uri) 取 DCR 客户端。
func (m *MCPManager) addClientCredentials(ctx context.Context, serverID string, tok *types.MCPOAuthTokenRow, form url.Values) error {
	if tok.ClientID == "" {
		return nil // 旧令牌行无客户端信息：按原样尝试，失败由调用方置需授权
	}
	form.Set("client_id", tok.ClientID)
	secretEnc, err := m.clientSecretEncFor(ctx, serverID, tok)
	if err != nil || secretEnc == "" {
		return err
	}
	secret, err := m.cipher.Decrypt(secretEnc)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp oauth: decrypt client secret", err)
	}
	form.Set("client_secret", secret)
	return nil
}

func (m *MCPManager) clientSecretEncFor(ctx context.Context, serverID string, tok *types.MCPOAuthTokenRow) (string, error) {
	row, err := m.rowRepo.GetMCPServer(ctx, serverID)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: load server for refresh", err)
	}
	if row != nil {
		if cfg, perr := parseRowOAuthConfig(row.OAuth); perr == nil && cfg != nil && cfg.ClientID == tok.ClientID {
			return cfg.ClientSecretEnc, nil
		}
	}
	cl, err := m.rowRepo.GetMCPOAuthClient(ctx, tok.Issuer, tok.RedirectURI)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: load registered client", err)
	}
	if cl == nil || cl.ClientID != tok.ClientID {
		return "", nil // CIMD 等公开客户端：无密钥
	}
	return cl.ClientSecretEnc, nil
}
