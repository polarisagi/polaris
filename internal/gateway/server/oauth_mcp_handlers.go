package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// MCP OAuth 网关入口（8e-2）：授权服务器回调页 + Client ID Metadata Document。
// 两者都在鉴权中间件的精确豁免白名单里（见 middleware_auth.go
// oauthPublicGetPathSet），路径与 internal/extension/mcp/oauth_register.go
// gatewayCallbackURL / ClientMetadataDocument 硬编码的约定路径一致，不得改动
// 其一而不改另一。
// ============================================================================

// oauthCallbackNonceBytes CSP nonce 随机字节数（128 bit）。
const oauthCallbackNonceBytes = 16

// HandleMCPOAuthCallback 处理 MCP OAuth 授权回调（GET /oauth/mcp/callback）。
//
// 浏览器从第三方授权服务器跳转回来，不带 Polaris 令牌——鉴权豁免依据是 state
// 一次性凭据本身（见 CompleteAuthorization），而不是路径隐式豁免。
//
// 安全要点：
//   - 响应头 Cache-Control: no-store（回调 URL 含一次性授权码/state，不得被缓存）、
//     Referrer-Policy: no-referrer（不得把带 code 的 URL 泄露给下一跳的 Referer）。
//   - 严格 CSP，仅放行本页内联的那一段脚本（nonce），其余一律拒绝。
//   - 失败时只回传内部错误分类码（apperr.Code），绝不回显授权服务器给出的
//     error/error_description 原文——RFC 9207 iss 不匹配（混淆攻击）场景下，
//     error_description 可能是攻击者构造的诱导文案；统一不回显最简单也最安全。
func (s *Server) HandleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")

	nonce, err := randomHexNonce(oauthCallbackNonceBytes)
	if err != nil {
		// crypto/rand 故障极罕见；无 nonce 也不能放宽 CSP，直接给不含内联脚本的失败页。
		slog.Error("mcp oauth: callback nonce generation failed", "err", err)
		writeOAuthCallbackHTML(w, "", "", false)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'")

	if s.mcpMgr == nil {
		writeOAuthCallbackHTML(w, nonce, string(apperr.CodeUnimplemented), false)
		return
	}
	serverID, err := s.mcpMgr.CompleteAuthorization(r.Context(), r.URL.Query())
	if err != nil {
		// 只记服务端日志（排障用，日志不面向浏览器），不把 err.Error() 写进响应体——
		// 它可能内嵌授权服务器的 error_description（见 oauth_flow.go CompleteAuthorization）。
		slog.Warn("mcp oauth: callback failed", "server_id", serverID, "err", err)
		writeOAuthCallbackHTML(w, nonce, string(apperr.CodeOf(err)), false)
		return
	}
	writeOAuthCallbackHTML(w, nonce, serverID, true)
}

// randomHexNonce 生成 n 字节的 crypto/rand 随机值，十六进制编码，用作 CSP nonce。
func randomHexNonce(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp oauth: crypto/rand", err)
	}
	return hex.EncodeToString(b), nil
}

// writeOAuthCallbackHTML 渲染回调页极简 HTML。success=true 时 detail 是 serverID，
// 页面内联脚本向 opener postMessage 通知授权完成；否则 detail 是内部错误分类码，
// 页面只展示通用失败文案 + 错误码，不含脚本（无需向 opener 通知失败，弹窗关闭本身
// 就是前端轮询的信号，见 web/src/js/store/plugins.js authorizeConnector）。
func writeOAuthCallbackHTML(w http.ResponseWriter, nonce, detail string, success bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !success {
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head>`+
			`<body>%s</body></html>`,
			"授权失败", "授权未完成，请返回重试（错误码："+html.EscapeString(detail)+"）")
		return
	}

	sidJSON, err := json.Marshal(detail)
	if err != nil {
		sidJSON = []byte(`null`)
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head>`+
		`<body>%s<script nonce="%s">window.opener&&window.opener.postMessage({type:"polaris-mcp-oauth",serverId:%s},location.origin);</script></body></html>`,
		"授权完成", "授权完成，可关闭此窗口", nonce, sidJSON)
}

// HandleMCPOAuthClientMetadata 返回 Client ID Metadata Document（GET /oauth/client-metadata.json）。
//
// 仅当网关能以 https 且非回环基址被外部授权服务器访问时才有意义——本地/回环部署
// 时这份文档谁都访问不到，返回 404 而不是一份没有实际作用的文档
// （basic_authorization_client-registration.md §Client ID Metadata Documents）。
func (s *Server) HandleMCPOAuthClientMetadata(w http.ResponseWriter, r *http.Request) {
	base := httputil.ResolvePublicBase(r)
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || isLoopbackHost(u.Host) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	httputil.WriteJSON(w, mcp.ClientMetadataDocument(base))
}
