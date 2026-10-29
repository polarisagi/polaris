package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// oauthFlowTTL 一次授权流程（state → 记录）的最大存活时间。
// HE-6 例外：这段状态本就是一次性、短生命周期的临时凭据（code_verifier/state），落盘持久化
// 反而扩大泄露面且没有收益——进程重启后用户重新点一次"授权"即可重新发起，成本等同于
// 用户中途关闭浏览器标签页这种本就会发生的正常路径，不构成状态丢失。
const oauthFlowTTL = 10 * time.Minute

// oauthFlowState 一次进行中的授权流程记录（BeginAuthorization 写入，CompleteAuthorization
// 消费）。ClientSecret 为明文，仅内存持有，流程完成或过期即被整体丢弃；不落库、不记日志。
type oauthFlowState struct {
	ServerID          string
	Issuer            string
	IssParamSupported bool
	Verifier          string
	RedirectURI       string
	ClientID          string
	ClientSecret      string
	TokenEndpoint     string
	Resource          string
	Scopes            []string
	CreatedAt         time.Time
}

// oauthBroker 管理进行中的授权流程：state → *oauthFlowState，一次性取出（无论成功与否
// 都会被删除，防止授权码/state 被重放）。
type oauthBroker struct {
	mu    sync.Mutex
	flows map[string]*oauthFlowState
}

func newOAuthBroker() *oauthBroker { return &oauthBroker{flows: map[string]*oauthFlowState{}} }

func (b *oauthBroker) put(state string, fs *oauthFlowState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcLocked()
	b.flows[state] = fs
}

// takeValid 一次性取出并删除指定 state 的流程记录；不存在或已过期均返回错误。
func (b *oauthBroker) takeValid(state string) (*oauthFlowState, error) {
	b.mu.Lock()
	fs, ok := b.flows[state]
	if ok {
		delete(b.flows, state)
	}
	b.mu.Unlock()
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "mcp oauth: unknown or already-used state")
	}
	if time.Since(fs.CreatedAt) > oauthFlowTTL {
		return nil, apperr.New(apperr.CodeTimeout, "mcp oauth: authorization flow expired, please retry")
	}
	return fs, nil
}

func (b *oauthBroker) gcLocked() {
	cutoff := time.Now().Add(-oauthFlowTTL)
	for k, v := range b.flows {
		if v.CreatedAt.Before(cutoff) {
			delete(b.flows, k)
		}
	}
}

// BeginAuthorization 发起一次授权流程：发现 → 客户端注册 → PKCE/state → 记录流程 →
// 返回授权 URL（basic_authorization.md §Authorization Flow Steps）。
func (m *MCPManager) BeginAuthorization(ctx context.Context, serverID, redirectBase string) (string, error) {
	if m.rowRepo == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp oauth: row source not configured")
	}
	row, err := m.rowRepo.GetMCPServer(ctx, serverID)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: get server", err)
	}
	if row == nil {
		return "", apperr.New(apperr.CodeNotFound, "mcp oauth: server not found: "+serverID)
	}
	if normalizeRowTransport(row.Transport) == MCPStdio {
		return "", apperr.New(apperr.CodeInvalidInput, "mcp oauth: not applicable to stdio transport")
	}

	resource, err := canonicalResourceURI(row.URL)
	if err != nil {
		return "", err
	}
	cachedRM, cachedScope := m.cachedChallenge(serverID)
	disc, err := m.discoverOAuth(ctx, row.URL, cachedRM)
	if err != nil {
		return "", err
	}

	existingScopes := m.reconcileTokenIssuer(ctx, serverID, disc.Issuer)
	scopes := selectScopes(cachedScope, disc.PRMScopes, existingScopes, disc.ASMeta.ScopesSupported)

	client, err := m.resolveClient(ctx, *row, disc, redirectBase)
	if err != nil {
		return "", err
	}

	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	redirectURI, err := gatewayCallbackURL(redirectBase)
	if err != nil {
		return "", err
	}

	m.oauthBroker.put(state, &oauthFlowState{
		ServerID: serverID, Issuer: disc.Issuer, IssParamSupported: disc.ASMeta.AuthorizationResponseIssParameterSupported,
		Verifier: verifier, RedirectURI: redirectURI, ClientID: client.ClientID, ClientSecret: client.ClientSecret,
		TokenEndpoint: disc.ASMeta.TokenEndpoint, Resource: resource, Scopes: scopes, CreatedAt: time.Now(),
	})

	authURL := buildAuthorizationURL(disc.ASMeta.AuthorizationEndpoint, client.ClientID, redirectURI, pkceChallengeS256(verifier), state, resource, scopes)
	slog.Info("mcp oauth: authorization flow started", "server_id", serverID, "issuer", disc.Issuer, "client_registration", client.Method, "scope_count", len(scopes))
	return authURL, nil
}

// cachedChallenge 读取最近一次连接阶段挑战缓存的 (resource_metadata, scope)，供 BeginAuthorization
// 跳过重复的无令牌探测请求（basic_authorization.md 流程图允许直接复用已收到的挑战）。
func (m *MCPManager) cachedChallenge(serverID string) (resourceMetadata, scope string) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok {
		return "", ""
	}
	return e.cachedChallenge()
}

// reconcileTokenIssuer basic_authorization_client-registration.md §Authorization Server
// Binding：发现得到的 issuer 与已存令牌 issuer 不同即视为授权服务器已变更，丢弃旧令牌
// （不复用旧 DCR 客户端不需要额外代码——mcp_oauth_clients 按 issuer 键天然隔离）。
// 返回值：issuer 未变时返回已存令牌的 scope（供 step-up 并集），否则返回 nil。
func (m *MCPManager) reconcileTokenIssuer(ctx context.Context, serverID, issuer string) []string {
	if m.rowRepo == nil {
		return nil
	}
	tok, err := m.rowRepo.GetMCPOAuthToken(ctx, serverID)
	if err != nil || tok == nil {
		return nil
	}
	if tok.Issuer == issuer {
		return strings.Fields(tok.Scopes)
	}
	slog.Warn("mcp oauth: authorization server changed, discarding stale token", "server_id", serverID)
	if err := m.rowRepo.DeleteMCPOAuthToken(ctx, serverID); err != nil {
		slog.Warn("mcp oauth: failed to delete stale token", "server_id", serverID)
	}
	return nil
}

// selectScopes basic_authorization.md §Scope Selection Strategy + §Step-Up Authorization Flow：
// 挑战 scope 优先于 PRM scopes_supported；step-up 时与已存令牌 scope 取并集；AS 声明支持
// offline_access 时追加（MAY）。
func selectScopes(challengeScope string, prmScopes, existingScopes, asScopesSupported []string) []string {
	var base []string
	switch {
	case challengeScope != "":
		base = strings.Fields(challengeScope)
	case len(prmScopes) > 0:
		base = prmScopes
	}
	merged := mergeScopeLists(base, existingScopes)
	if containsString(asScopesSupported, "offline_access") {
		merged = mergeScopeLists(merged, []string{"offline_access"})
	}
	return merged
}

// mergeScopeLists 合并两个 scope 列表并去重，保留首次出现顺序（与 pluginspec 包同名函数
// 逻辑一致，按各自消费方独立实现，避免为一个几行函数建立跨包依赖）。
func mergeScopeLists(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string(nil), a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// buildAuthorizationURL 组装授权请求 URL：PKCE S256 + resource（RFC 8707）+ state + scope。
func buildAuthorizationURL(endpoint, clientID, redirectURI, codeChallenge, state, resource string, scopes []string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {resource},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + q.Encode()
}

// randomURLSafe 生成 nBytes 字节的 crypto/rand 随机值，base64url（无填充）编码，
// 用于 code_verifier 与 state（basic_authorization_security.md：state/verifier 须密码学随机）。
func randomURLSafe(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: crypto/rand", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallengeS256 OAuth 2.1 §7.5.2：code_challenge = BASE64URL(SHA256(code_verifier))。
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// tokenResponse 令牌端点响应（RFC 6749 §5.1，授权码交换与刷新共用）。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// CompleteAuthorization 处理授权回调：state 一次性取出 → iss 校验（RFC 9207）→ 读取 error →
// 授权码换令牌（含 code_verifier + resource）→ 加密落库 → StartFromDB 重连。
// 返回值第一个字段始终是 ServerID（即使发生错误也返回，供调用方定位是哪个服务器失败）。
func (m *MCPManager) CompleteAuthorization(ctx context.Context, query url.Values) (string, error) {
	state := query.Get("state")
	if state == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "mcp oauth: missing state")
	}
	fs, err := m.oauthBroker.takeValid(state)
	if err != nil {
		return "", err
	}

	if err := validateIssResponse(fs, query); err != nil {
		slog.Warn("mcp oauth: iss validation failed", "server_id", fs.ServerID)
		return fs.ServerID, err
	}
	if oauthErr := query.Get("error"); oauthErr != "" {
		return fs.ServerID, apperr.New(apperr.CodeInvalidInput,
			"mcp oauth: authorization server returned error: "+oauthErr+" "+query.Get("error_description"))
	}
	code := query.Get("code")
	if code == "" {
		return fs.ServerID, apperr.New(apperr.CodeInvalidInput, "mcp oauth: missing authorization code")
	}

	tok, err := m.exchangeCode(ctx, fs, code)
	if err != nil {
		return fs.ServerID, err
	}
	if err := m.storeToken(ctx, fs, tok); err != nil {
		return fs.ServerID, err
	}
	if e := m.entryFor(fs.ServerID); e != nil {
		e.clearAuthRequired()
	}
	slog.Info("mcp oauth: authorization completed", "server_id", fs.ServerID, "issuer", fs.Issuer)

	if err := m.StartFromDB(ctx, fs.ServerID); err != nil {
		return fs.ServerID, apperr.Wrap(apperr.CodeOf(err), "mcp oauth: reconnect after authorization", err)
	}
	return fs.ServerID, nil
}

// validateIssResponse RFC 9207 §2.4 四种情形（basic_authorization.md 表格），iss 用简单字符串
// 比较，禁止任何规范化（大小写折叠/端口省略/末尾斜杠/百分号编码）。iss 不匹配时调用方不得
// 读取/展示 error/error_description——本函数只做校验，不读取 error，避免误用顺序。
func validateIssResponse(fs *oauthFlowState, query url.Values) error {
	iss := query.Get("iss")
	switch {
	case fs.IssParamSupported && iss == "":
		return apperr.New(apperr.CodeUnauthorized, "mcp oauth: authorization server advertises iss support but response omitted iss (RFC 9207)")
	case iss != "" && iss != fs.Issuer:
		return apperr.New(apperr.CodeUnauthorized, "mcp oauth: iss mismatch, possible mix-up attack")
	default:
		return nil
	}
}

// exchangeCode 用授权码 + code_verifier + resource 换取令牌（basic_authorization.md
// §Resource Parameter Implementation：resource 必须同时出现在授权与令牌请求中）。
func (m *MCPManager) exchangeCode(ctx context.Context, fs *oauthFlowState, code string) (*tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {fs.RedirectURI},
		"client_id":     {fs.ClientID},
		"code_verifier": {fs.Verifier},
		"resource":      {fs.Resource},
	}
	if fs.ClientSecret != "" {
		form.Set("client_secret", fs.ClientSecret)
	}
	resp, err := postForm[tokenResponse](ctx, m.httpClient, fs.TokenEndpoint, form)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: token exchange failed", err)
	}
	if resp.AccessToken == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: token endpoint returned empty access_token")
	}
	return resp, nil
}

// storeToken 加密落库首次获取的令牌；cipher 未配置时 fail-closed 拒绝，不落明文。
func (m *MCPManager) storeToken(ctx context.Context, fs *oauthFlowState, tok *tokenResponse) error {
	if m.cipher == nil {
		return apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured, refusing to store token in plaintext")
	}
	accessEnc, err := m.cipher.Encrypt(tok.AccessToken)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp oauth: encrypt access token", err)
	}
	refreshEnc := ""
	if tok.RefreshToken != "" {
		refreshEnc, err = m.cipher.Encrypt(tok.RefreshToken)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "mcp oauth: encrypt refresh token", err)
		}
	}
	scopes := tok.Scope
	if scopes == "" {
		scopes = strings.Join(fs.Scopes, " ")
	}
	expiresAt := ""
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	row := types.MCPOAuthTokenRow{
		ServerID: fs.ServerID, Issuer: fs.Issuer, Resource: fs.Resource,
		ClientID: fs.ClientID, RedirectURI: fs.RedirectURI,
		AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc,
		TokenType: firstNonEmptyStr(tok.TokenType, "Bearer"), Scopes: scopes, ExpiresAt: expiresAt,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.rowRepo.UpsertMCPOAuthToken(ctx, row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp oauth: persist token", err)
	}
	return nil
}

// entryFor 只读查找 entries 表中的 entry，不存在返回 nil。
func (m *MCPManager) entryFor(serverID string) *mcpEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entries[serverID]
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
