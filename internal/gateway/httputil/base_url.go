package httputil

import (
	"net/http"
	"strings"
)

// ResolveRedirectBase 计算当前请求对应的网关外部基址：浏览器请求携带 Origin 头时
// 采用 Origin（鉴权中间件的同源校验已保证 Cookie 请求的 Origin 与 Host 一致，见
// internal/gateway/server/middleware_auth.go checkOrigin）；否则退回 scheme://Host，
// scheme 由请求是否 TLS 判定。
//
// MCP OAuth 授权 URL 的 redirect_uri 与 CIMD 文档基址两处都要求同一条推导规则，
// 收敛到这里防止各自实现漂移（分别位于 sysadmin/mcpadmin 与 gateway/server 两个包，
// 不共享同一父包，故落在双方都已依赖的 httputil）。
func ResolveRedirectBase(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		return strings.TrimSuffix(strings.TrimSpace(origin), "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ResolvePublicBase 公开文档（CIMD）的外部基址：由授权服务器服务端抓取，请求不带 Origin；
// TLS 终结在反向代理时 r.TLS 为空，须采信 X-Forwarded-Proto / X-Forwarded-Host，否则
// 授权阶段按浏览器 Origin 选定的 https client_id 在抓取时算成 http 而 404，CIMD 在代理后
// 永远不可用。只用于公开只读文档：伪造转发头只影响伪造者自己拿到的内容；授权跳转地址
// 仍用 ResolveRedirectBase（Origin 经同源校验），不采信转发头。
func ResolvePublicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := firstForwarded(r.Header.Get("X-Forwarded-Proto")); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := firstForwarded(r.Header.Get("X-Forwarded-Host")); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// firstForwarded 多级代理时转发头为逗号分隔列表，首项是离客户端最近的一跳。
func firstForwarded(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}
