package network

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// Capability 出口强制检查 + TaintEgressCheck + ValidateGitURL(SSRF 预检) +
// 错误类型定义（R7 拆分自 safe_dialer.go）。
// SafeDialer 结构体/DialContext/DNS 解析/CIDR 校验见 safe_dialer.go。
// ============================================================================

// TaintEgressCheck 出口拦截: payload 中任一字段 TaintLevel ≥ TaintMedium
// 且未经 SanitizeByUserReview → 拒绝出站。
func (sd *SafeDialer) TaintEgressCheck(taintLevels []types.TaintLevel) error {
	for _, tl := range taintLevels {
		if tl >= types.TaintMedium {
			return &ErrDialerTaintBlocked{Level: tl}
		}
	}
	return nil
}

// Capability 出口强制检查。
type Capability int

const (
	CapNetworkRead       Capability = iota // 仅 GET/HEAD
	CapNetworkWriteLocal                   // 仅内网 POST/PUT
	CapNetworkWrite                        // 全网络
)

// CheckCapability Phase 0: Capability Token 出口强制。
func CheckCapability(cap Capability, method string) error {
	switch cap {
	case CapNetworkRead:
		if !isReadOnlyHTTP(method) {
			return &ErrCapabilityWriteBlocked{Method: method}
		}
	case CapNetworkWriteLocal:
		// 调用方负责在 DialContext 中校验 IP 为内网地址
	case CapNetworkWrite:
		// 放行，后续 Phase 1-4 保护
	}
	return nil
}

func isReadOnlyHTTP(method string) bool {
	m := strings.ToUpper(method)
	return m == "GET" || m == "HEAD" || m == "OPTIONS"
}

// CapabilityRoundTripper 包装 http.RoundTripper 增加能力校验。
type CapabilityRoundTripper struct {
	inner http.RoundTripper
	cap   Capability
}

func (c *CapabilityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := CheckCapability(c.cap, req.Method); err != nil {
		return nil, err
	}
	res, err := c.inner.RoundTrip(req)
	if err != nil {
		// 走到这里说明 Capability 检查已通过（上面已 return），这是内层 RoundTripper
		// 自身的传输失败（DNS/连接/TLS 等），与能力校验无关——错误信息不应写成
		// "capability check failed" 误导排障（此前的措辞会让正常网络故障被误判为
		// 安全拦截）。
		return res, apperr.Wrap(apperr.CodeInternal, "CapabilityRoundTripper: inner round tripper transport failed", err)
	}
	return res, nil
}

// WrapCapability 将 CapabilityRoundTripper 包装到现有的 RoundTripper 上。
func WrapCapability(inner http.RoundTripper, cap Capability) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &CapabilityRoundTripper{
		inner: inner,
		cap:   cap,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return apperr.Wrap(apperr.CodeInternal, "sleepCtx", ctx.Err())
	}
}

// ValidateGitURL 解析 rawURL host 并验证其不落入受阻 CIDR（SSRF 预检）。
// 专为无法注入 DialContext 的子进程出站调用（如 git exec）设计，在执行前调用。
// 本地路径（无 host）直接放行；DNS 解析失败视为不可信拒绝。
func ValidateGitURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "safe_dialer: invalid git URL", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil // 本地路径（file:// 或相对路径），无出站连接
	}
	var r net.Resolver
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "safe_dialer: git URL DNS lookup failed", err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	for _, ip := range ips {
		for _, block := range getParsedCIDRs() {
			if block.Contains(ip) {
				return &SSRFBlockedError{Host: host, IPs: ips}
			}
		}
	}
	return nil
}

// ============================================================================
// 错误类型
// ============================================================================

type SSRFBlockedError struct {
	Host string
	IPs  []net.IP
}

func (e *SSRFBlockedError) Error() string {
	return fmt.Sprintf("safe_dialer: ssrf blocked — host %s resolves to blocked CIDR", e.Host)
}

type ErrDNSResponseTooLarge struct {
	Host  string
	Count int
}

func (e *ErrDNSResponseTooLarge) Error() string {
	return fmt.Sprintf("safe_dialer: dns response too large for %s (%d ips)", e.Host, e.Count)
}

type ErrDialerTaintBlocked struct {
	Level types.TaintLevel
}

func (e *ErrDialerTaintBlocked) Error() string {
	return fmt.Sprintf("safe_dialer: taint level %s blocked egress (requires SanitizeByUserReview)", e.Level.String())
}

type ErrCapabilityWriteBlocked struct {
	Method string
}

func (e *ErrCapabilityWriteBlocked) Error() string {
	return fmt.Sprintf("safe_dialer: capability read_only blocked write method %s", e.Method)
}

// ErrQUICDisabled QUIC/HTTP3 被禁用时返回的错误。
type ErrQUICDisabled struct{}

func (e *ErrQUICDisabled) Error() string {
	return "safe_dialer: QUIC/HTTP3 disabled — use TCP via DialContext"
}

// ErrNonLoopbackBlocked local_only 模式下非 loopback IP 被拒绝。
type ErrNonLoopbackBlocked struct {
	IP net.IP
}

func (e *ErrNonLoopbackBlocked) Error() string {
	return fmt.Sprintf("safe_dialer: non-loopback IP %s blocked (local_only mode)", e.IP.String())
}
