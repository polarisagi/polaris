package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// protectedResourceMetadata OAuth 2.0 Protected Resource Metadata（RFC 9728）的最小必要字段。
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// authServerMetadata OAuth 2.0 Authorization Server Metadata（RFC 8414）/ OpenID Connect
// Discovery 1.0 的最小必要字段并集。
type authServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RegistrationEndpoint                       string   `json:"registration_endpoint"`
	ScopesSupported                            []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// oauthDiscovery 一次授权流程所需的发现结果最小集合。
type oauthDiscovery struct {
	Issuer    string
	ASMeta    *authServerMetadata
	PRMScopes []string
}

// discoverOAuth 按 basic_authorization_discovery.md 顺序执行 PRM → 授权服务器选择 → AS 元数据
// 三步发现。cachedResourceMetadataURL 非空时跳过 well-known 探测，直接使用 401 挑战给出的 URL。
func (m *MCPManager) discoverOAuth(ctx context.Context, mcpServerURL, cachedResourceMetadataURL string) (*oauthDiscovery, error) {
	u, err := url.Parse(mcpServerURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: invalid MCP server url")
	}
	allowHTTP := isLoopbackURL(u)

	prm, err := fetchProtectedResourceMetadata(ctx, m.httpClient, u, cachedResourceMetadataURL, allowHTTP)
	if err != nil {
		return nil, err
	}
	if err := validatePRMResource(prm, u); err != nil {
		return nil, err
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: protected resource metadata has no authorization_servers")
	}
	issuer := prm.AuthorizationServers[0]

	asMeta, err := discoverAuthServerMetadata(ctx, m.httpClient, issuer, allowHTTP)
	if err != nil {
		return nil, err
	}
	slog.Info("mcp oauth: discovery complete", "issuer", issuer)
	return &oauthDiscovery{Issuer: issuer, ASMeta: asMeta, PRMScopes: prm.ScopesSupported}, nil
}

// protectedResourceWellKnownURLs basic_authorization_discovery.md §Protected Resource Metadata
// Discovery Requirements：先带路径的子路径 well-known，再回退根 well-known。
func protectedResourceWellKnownURLs(mcpURL *url.URL) []string {
	base := mcpURL.Scheme + "://" + mcpURL.Host
	var urls []string
	if mcpURL.Path != "" && mcpURL.Path != "/" {
		urls = append(urls, base+"/.well-known/oauth-protected-resource"+mcpURL.Path)
	}
	urls = append(urls, base+"/.well-known/oauth-protected-resource")
	return urls
}

// fetchProtectedResourceMetadata resourceMetadataURL 非空（来自 WWW-Authenticate 挑战）时只取该
// URL；否则按 well-known 顺序回退尝试。所有候选 URL 必须 https，除非 MCP 服务器本身是回环地址。
func fetchProtectedResourceMetadata(ctx context.Context, hc network.SafeHTTPClient, mcpURL *url.URL, resourceMetadataURL string, allowHTTP bool) (*protectedResourceMetadata, error) {
	candidates := []string{resourceMetadataURL}
	if resourceMetadataURL == "" {
		candidates = protectedResourceWellKnownURLs(mcpURL)
	}
	var lastErr error
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if err := requireHTTPSOrLoopback(c, allowHTTP); err != nil {
			lastErr = err
			continue
		}
		doc, err := getJSON[protectedResourceMetadata](ctx, hc, c)
		if err != nil {
			lastErr = err
			continue
		}
		return doc, nil
	}
	if lastErr == nil {
		lastErr = apperr.New(apperr.CodeNotFound, "mcp oauth: no protected resource metadata candidate")
	}
	return nil, apperr.Wrap(apperr.CodeNotFound, "mcp oauth: protected resource metadata discovery failed", lastErr)
}

// validatePRMResource 校验 PRM 的 resource 字段与被访问的 MCP 服务器 URL 一致（按 RFC 8707 §2
// 规范形式比较），防止把令牌绑定到错误的资源。resource 字段缺失时放行（部分实现省略非强制字段）。
func validatePRMResource(prm *protectedResourceMetadata, mcpURL *url.URL) error {
	if prm.Resource == "" {
		return nil
	}
	want, err := canonicalResourceURI(mcpURL.String())
	if err != nil {
		return err
	}
	got, err := canonicalResourceURI(prm.Resource)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: protected resource metadata has invalid resource", err)
	}
	if got != want {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mcp oauth: protected resource metadata resource %q does not match server %q", got, want))
	}
	return nil
}

// authServerWellKnownURLs basic_authorization_discovery.md §Authorization Server Metadata
// Discovery：无路径 issuer 试 2 个端点；带路径 issuer 按插入/追加规则试 3 个端点，顺序固定。
func authServerWellKnownURLs(issuer string) ([]string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: invalid issuer url "+issuer)
	}
	base := u.Scheme + "://" + u.Host
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return []string{
			base + "/.well-known/oauth-authorization-server",
			base + "/.well-known/openid-configuration",
		}, nil
	}
	return []string{
		base + "/.well-known/oauth-authorization-server/" + path,
		base + "/.well-known/openid-configuration/" + path,
		base + "/" + path + "/.well-known/openid-configuration",
	}, nil
}

// discoverAuthServerMetadata 按优先顺序尝试 well-known 端点；每个候选文档的 issuer 必须与传入的
// issuer 逐字节相等（RFC 8414 §3.3 / OIDC Discovery §4.3），不等则跳过该候选而非直接失败。
func discoverAuthServerMetadata(ctx context.Context, hc network.SafeHTTPClient, issuer string, allowHTTP bool) (*authServerMetadata, error) {
	if err := requireHTTPSOrLoopback(issuer, allowHTTP); err != nil {
		return nil, err
	}
	urls, err := authServerWellKnownURLs(issuer)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, u := range urls {
		doc, err := getJSON[authServerMetadata](ctx, hc, u)
		if err != nil {
			lastErr = err
			continue
		}
		if doc.Issuer != issuer {
			lastErr = apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mcp oauth: issuer mismatch at %s (got %q want %q)", u, doc.Issuer, issuer))
			continue
		}
		if err := validateASMetadata(doc, allowHTTP); err != nil {
			return nil, err
		}
		return doc, nil
	}
	if lastErr == nil {
		lastErr = apperr.New(apperr.CodeNotFound, "mcp oauth: no authorization server metadata candidate")
	}
	return nil, apperr.Wrap(apperr.CodeNotFound, "mcp oauth: authorization server metadata discovery failed for issuer "+issuer, lastErr)
}

// validateASMetadata basic_authorization_security.md §Authorization Code Protection：缺少
// code_challenge_methods_supported（不支持 PKCE）一律拒绝；authorization_endpoint/token_endpoint
// 必须存在且为 https（回环 MCP 服务器例外）。
func validateASMetadata(doc *authServerMetadata, allowHTTP bool) error {
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return apperr.New(apperr.CodeInvalidInput, "mcp oauth: authorization server metadata missing required endpoints")
	}
	if !containsString(doc.CodeChallengeMethodsSupported, "S256") {
		return apperr.New(apperr.CodeInvalidInput, "mcp oauth: authorization server does not support PKCE S256, refusing")
	}
	for _, ep := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint} {
		if err := requireHTTPSOrLoopback(ep, allowHTTP); err != nil {
			return err
		}
	}
	if doc.RegistrationEndpoint != "" {
		if err := requireHTTPSOrLoopback(doc.RegistrationEndpoint, allowHTTP); err != nil {
			return err
		}
	}
	return nil
}

// requireHTTPSOrLoopback basic_authorization_security.md §Communication Security：所有
// AS/PRM 端点必须 https；仅当 MCP 服务器 URL 本身是回环地址（本机开发/测试）时放行 http。
func requireHTTPSOrLoopback(rawURL string, allowHTTP bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: invalid url "+rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if allowHTTP && u.Scheme == "http" {
		return nil
	}
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mcp oauth: url %q must use https", rawURL))
}

// canonicalResourceURI RFC 8707 §2 规范资源 URI：scheme/host 小写，去 fragment，去末尾 "/"
// （basic_authorization.md §Canonical Server URI：两种形式都合法，SHOULD 统一用无尾斜杠形式；
// 根路径 "/" 去掉尾斜杠后与省略路径语义等价，一并归一化），供 resource 参数与 PRM.resource 校验共用。
func canonicalResourceURI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "mcp oauth: invalid resource url "+raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
