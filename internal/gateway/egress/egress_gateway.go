// Package egress 实现 M13 §1.2.2 EgressGateway HTTP 层域名预检。
// 委托 M11 SafeDialer 执行完整 SSRF 校验；本层仅做预检减少 DNS 开销。
package egress

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// DefaultAllowedDomains 返回内置 Provider 域名白名单（无需配置即生效）。
func DefaultAllowedDomains() []string {
	return []string{
		"api.deepseek.com",
		"api.anthropic.com",
		"api.openai.com",
		"api.github.com",
		"github.com",
		"githubusercontent.com",
		"ghproxy.net",
		"mirror.ghproxy.com",
		"gh-proxy.com",
		"ghp.ci",
	}
}

// EgressGateway 实现 http.RoundTripper，在传递给底层 transport 前执行域名白名单预检。
// 并发安全：allowedDomains 通过原子替换更新，RoundTrip 仅读。
type EgressGateway struct {
	inner          http.RoundTripper // 下层 transport（SafeDialer-backed）
	allowedDomains atomic.Pointer[[]string]
	mu             sync.Mutex // 保护 AddAllowedDomain 的写操作
}

// NewEgressGateway 创建 EgressGateway，inner 为已配置 SafeDialer 的 *http.Client.Transport。
func NewEgressGateway(inner http.RoundTripper, allowedDomains []string) *EgressGateway {
	gw := &EgressGateway{inner: inner}
	domains := make([]string, len(allowedDomains))
	copy(domains, allowedDomains)
	gw.allowedDomains.Store(&domains)
	return gw
}

// RoundTrip 执行域名预检后委托底层 transport。
func (gw *EgressGateway) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if !gw.isAllowed(host) {
		return nil, apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("egress_gateway: domain %q not in allowlist (M13 §1.2.2)", host))
	}
	return gw.inner.RoundTrip(req)
}

// AddAllowedDomain 动态追加白名单域名（对应 `polaris config network allow <domain>`）。
func (gw *EgressGateway) AddAllowedDomain(domain string) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	current := *gw.allowedDomains.Load()
	for _, d := range current {
		if d == domain {
			return // 已存在
		}
	}
	next := make([]string, len(current)+1)
	copy(next, current)
	next[len(current)] = domain
	gw.allowedDomains.Store(&next)
}

func (gw *EgressGateway) isAllowed(host string) bool {
	host = strings.ToLower(host)
	domains := *gw.allowedDomains.Load()
	for _, d := range domains {
		d = strings.ToLower(d)
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
