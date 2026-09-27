package mcp

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// registeredClient 客户端注册解析结果。ClientSecret 为明文，仅在内存中随授权流程状态
// 短暂持有（oauthFlowState），从不落库明文、不进入日志（basic_authorization.md）。
type registeredClient struct {
	ClientID     string
	ClientSecret string
	Method       string // "preregistered" | "cimd" | "dcr"
}

// dcrResponse RFC 7591 §3.2.1 客户端注册响应的最小必要字段。
type dcrResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// gatewayCallbackURL 拼出网关托管的 OAuth 回调地址：<基址>/oauth/mcp/callback
// （8e-2 负责路由本身；本包只生成/消费这个约定 URL）。
func gatewayCallbackURL(redirectBase string) (string, error) {
	base := strings.TrimSuffix(strings.TrimSpace(redirectBase), "/")
	if base == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "mcp oauth: redirectBase must not be empty")
	}
	return base + "/oauth/mcp/callback", nil
}

// ClientMetadataDocument 生成 Client ID Metadata Document 内容（Polaris 作为网关托管方，
// 8e-2 在 GET <base>/oauth/client-metadata.json 返回本函数结果）。
// basic_authorization_client-registration.md §Client ID Metadata Documents：文档必须至少
// 含 client_id、client_name、redirect_uris；client_id 值须与文档 URL 完全一致。
func ClientMetadataDocument(base string) map[string]any {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	return map[string]any{
		"client_id":                  base + "/oauth/client-metadata.json",
		"client_name":                "Polaris",
		"redirect_uris":              []string{base + "/oauth/mcp/callback"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
}

// isLoopbackRedirect redirectBase 的 host 是否回环——决定 DCR application_type
// （native/web）与 CIMD 是否可用（basic_authorization_client-registration.md
// §Application Type and Redirect URI Constraints）。
func isLoopbackRedirect(redirectBase string) bool {
	u, err := url.Parse(redirectBase)
	if err != nil {
		return false
	}
	return isLoopbackURL(u)
}

// resolveClient 按 basic_authorization_client-registration.md 的优先级解析客户端：
// 预注册 > Client ID Metadata Document > Dynamic Client Registration > 报错。
func (m *MCPManager) resolveClient(ctx context.Context, row types.MCPServerRow, disc *oauthDiscovery, redirectBase string) (*registeredClient, error) {
	rowCfg, err := parseRowOAuthConfig(row.OAuth)
	if err != nil {
		return nil, err
	}
	if rowCfg != nil && rowCfg.ClientID != "" {
		return m.resolvePreregisteredClient(rowCfg, disc, row.ID)
	}

	redirectURI, err := gatewayCallbackURL(redirectBase)
	if err != nil {
		return nil, err
	}
	loopback := isLoopbackRedirect(redirectBase)

	if disc.ASMeta.ClientIDMetadataDocumentSupported && !loopback && strings.HasPrefix(redirectBase, "https://") {
		clientID := strings.TrimSuffix(redirectBase, "/") + "/oauth/client-metadata.json"
		slog.Info("mcp oauth: using client id metadata document", "server_id", row.ID, "issuer", disc.Issuer)
		return &registeredClient{ClientID: clientID, Method: "cimd"}, nil
	}

	if disc.ASMeta.RegistrationEndpoint != "" {
		return m.registerViaDCR(ctx, row.ID, disc, redirectURI, loopback)
	}

	return nil, apperr.New(apperr.CodeConflict, "authorization server requires a pre-registered client")
}

// resolvePreregisteredClient 行内 oauth.client_id 非空时的预注册路径。规范要求：若行还配置了
// auth_server_metadata_url，发现得到的 issuer 必须与其一致，否则视为配置漂移，surface 错误
// 而不是静默用错误凭据尝试授权（basic_authorization_client-registration.md §Authorization
// Server Binding）。
func (m *MCPManager) resolvePreregisteredClient(rowCfg *rowOAuthConfig, disc *oauthDiscovery, serverID string) (*registeredClient, error) {
	if rowCfg.AuthServerMetadataURL != "" && !issuerMatchesConfiguredMetadataURL(disc.Issuer, rowCfg.AuthServerMetadataURL) {
		return nil, apperr.New(apperr.CodeConflict,
			"mcp oauth: discovered issuer does not match server's configured auth_server_metadata_url")
	}
	secret := ""
	if rowCfg.ClientSecretEnc != "" {
		if m.cipher == nil {
			return nil, apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured, cannot decrypt pre-registered client secret")
		}
		s, err := m.cipher.Decrypt(rowCfg.ClientSecretEnc)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: decrypt pre-registered client secret", err)
		}
		secret = s
	}
	slog.Info("mcp oauth: using pre-registered client", "server_id", serverID, "issuer", disc.Issuer)
	return &registeredClient{ClientID: rowCfg.ClientID, ClientSecret: secret, Method: "preregistered"}, nil
}

// issuerMatchesConfiguredMetadataURL 行配置的 auth_server_metadata_url 是否与发现得到的
// issuer 属于同一授权服务器：取该 URL 的 scheme+host 与 issuer 的 scheme+host 比较
// （auth_server_metadata_url 本身是发现端点 URL 而非 issuer，不能直接逐字节比较）。
func issuerMatchesConfiguredMetadataURL(issuer, metadataURL string) bool {
	iu, err1 := url.Parse(issuer)
	mu, err2 := url.Parse(metadataURL)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(iu.Scheme, mu.Scheme) && strings.EqualFold(iu.Host, mu.Host)
}

// registerViaDCR RFC 7591 动态客户端注册：先查已注册客户端（按 issuer+redirect_uri 隔离，
// basic_authorization_client-registration.md §Authorization Server Binding），无则注册并持久化。
func (m *MCPManager) registerViaDCR(ctx context.Context, serverID string, disc *oauthDiscovery, redirectURI string, loopback bool) (*registeredClient, error) {
	if m.rowRepo == nil {
		return nil, apperr.New(apperr.CodeInternal, "mcp oauth: repo not configured")
	}
	existing, err := m.rowRepo.GetMCPOAuthClient(ctx, disc.Issuer, redirectURI)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: lookup dcr client", err)
	}
	if existing != nil {
		return m.reuseDCRClient(existing)
	}

	appType := "web"
	if loopback {
		appType = "native"
	}
	reqBody := map[string]any{
		"client_name":                "Polaris",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           appType,
	}
	resp, err := postJSON[dcrResponse](ctx, m.httpClient, disc.ASMeta.RegistrationEndpoint, reqBody)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: dynamic client registration failed", err)
	}
	if resp.ClientID == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: dynamic client registration returned empty client_id")
	}

	secretEnc := ""
	if resp.ClientSecret != "" {
		if m.cipher == nil {
			return nil, apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured, cannot store dcr client secret")
		}
		secretEnc, err = m.cipher.Encrypt(resp.ClientSecret)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: encrypt dcr client secret", err)
		}
	}
	row := types.MCPOAuthClientRow{Issuer: disc.Issuer, RedirectURI: redirectURI, ClientID: resp.ClientID,
		ClientSecretEnc: secretEnc, RegistrationMethod: "dcr", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := m.rowRepo.UpsertMCPOAuthClient(ctx, row); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: persist dcr client", err)
	}
	slog.Info("mcp oauth: dynamic client registration succeeded", "server_id", serverID, "issuer", disc.Issuer, "application_type", appType)
	return &registeredClient{ClientID: resp.ClientID, ClientSecret: resp.ClientSecret, Method: "dcr"}, nil
}

// reuseDCRClient 复用 mcp_oauth_clients 里已登记的客户端（同一 issuer+redirect_uri）。
func (m *MCPManager) reuseDCRClient(existing *types.MCPOAuthClientRow) (*registeredClient, error) {
	secret := ""
	if existing.ClientSecretEnc != "" {
		if m.cipher == nil {
			return nil, apperr.New(apperr.CodeInternal, "mcp oauth: credential cipher not configured, cannot decrypt dcr client secret")
		}
		s, err := m.cipher.Decrypt(existing.ClientSecretEnc)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: decrypt dcr client secret", err)
		}
		secret = s
	}
	return &registeredClient{ClientID: existing.ClientID, ClientSecret: secret, Method: "dcr"}, nil
}
