package substrate

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// blockedCIDRs 内网地址段 + loopback + link-local.
// 架构文档: docs/arch/M11-Policy-Safety.md §6
var blockedCIDRs = []string{
	"0.0.0.0/8",      // This Network（RFC 1122）
	"127.0.0.0/8",    // Loopback
	"10.0.0.0/8",     // RFC 1918 私有
	"172.16.0.0/12",  // RFC 1918 私有
	"192.168.0.0/16", // RFC 1918 私有
	"100.64.0.0/10",  // CGNAT 共享地址（RFC 6598）：云平台内部 LB/元数据常见范围
	"169.254.0.0/16", // Link-local（AWS/GCP/Azure 实例元数据 169.254.169.254）
	"::1/128",        // IPv6 Loopback
	"fc00::/7",       // IPv6 唯一本地地址
	"fe80::/10",      // IPv6 Link-local
}

// parsedBlockedCIDRs 是 blockedCIDRs 预编译后的 *net.IPNet 列表，避免每次调用重新 ParseCIDR。
// 预编译在包初始化时完成，运行期只读，无需加锁。
var parsedBlockedCIDRs []*net.IPNet

func init() {
	for _, cidr := range blockedCIDRs {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("safe_dialer: invalid built-in CIDR " + cidr + ": " + err.Error())
		}
		parsedBlockedCIDRs = append(parsedBlockedCIDRs, block)
	}
}

// SafeDialer 统一安全拨号器 —— SSRFGuard 五阶段校验的唯一实现。
// 实现 internal/protocol/interfaces.go (SafeDialer)。
// 所有出站网络连接必须通过此入口，CI safe_dialer_lint 扫描裸 net.Dial/grpc.Dial/http.Get → ERROR。
type SafeDialer struct {
	dnsCache    map[string][]net.IP // hostname → resolved IPs
	dnsCacheTTL time.Duration       // 默认 30s
	dnsCacheMu  sync.RWMutex
	dnsCacheTs  map[string]time.Time

	// QUIC/HTTP3 已禁用 — 禁止 UDP 绕过 DialContext。
	// Go net/http 默认不启用 QUIC；quic-go 通过 dialer.Control 在 SafeDialer 中显式拒绝 UDP。
	quicDisabled bool

	// localOnlyIPFilter local_only 模式下的 IP 过滤函数。
	// nil = 未启用；非 nil = 仅允许 filter(ip)==true 的 IP。
	localOnlyIPFilter func(net.IP) bool
}

var _ protocol.SafeDialer = (*SafeDialer)(nil)

// NewSafeDialer 初始化安全拨号器。
func NewSafeDialer() *SafeDialer {
	return &SafeDialer{
		dnsCache:     make(map[string][]net.IP),
		dnsCacheTTL:  30 * time.Second,
		dnsCacheTs:   make(map[string]time.Time),
		quicDisabled: true, // 禁用 QUIC/HTTP3 — 防止 UDP 绕过 DialContext
	}
}

// NewSafeHTTPClient 返回一个绑定了 SafeDialer 的 *http.Client。
// 所有 Adapter 和工具调用须使用此工厂，禁止传入 http.DefaultClient。
func NewSafeHTTPClient(sd *SafeDialer) *http.Client {
	if sd == nil {
		sd = NewSafeDialer()
	}
	transport := &http.Transport{
		DialContext:         sd.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// 只限制等待响应头的时间；body 读取由各 adapter 的 context 控制，
		// 不在此设全局 Timeout，否则 30s 后强制断流导致前端对话卡死。
		ResponseHeaderTimeout: 30 * time.Second,
	}
	// 禁用 HTTP/3: 仅允许 h2 和 http/1.1
	transport.TLSClientConfig = &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
	}
	return &http.Client{
		Transport: transport,
	}
}

// DialContext 执行 SSRFGuard 五阶段校验后建立连接。
// Phase 0: Capability Token 出口强制（调用方在调用前通过 Caller Capability 校验）
// Phase 1: DNS 解析 hostname → ips1
// Phase 2: 遍历 ips1，命中 blockedCIDRs → 拒绝
// Phase 3: 50ms TOCTOU 延迟后二次 DNS 解析 → ips2，重新 blockedCIDRs 校验
// Phase 3.5: len(ips2) > 20 → 拒绝
// Phase 4: 验证通过后锁定 ips2 中首个非阻塞 IP 建立连接
func (sd *SafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// QUIC/HTTP3 阻断: 拒绝 UDP 连接
	if sd.quicDisabled && strings.EqualFold(network, "udp") {
		return nil, &ErrQUICDisabled{}
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = address
		port = "443"
	}

	// Phase 1: DNS 解析
	ips1, err := sd.resolveDNS(ctx, host)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "safe_dialer phase1 dns", err)
	}

	// Phase 2: blockedCIDRs 校验
	if sd.containsBlockedCIDR(ips1) {
		return nil, &SSRFBlockedError{Host: host, IPs: ips1}
	}

	// Phase 3: 50ms TOCTOU 延迟 + 二次 DNS 解析（强制绕过缓存，TOCTOU 保护）
	if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
		return nil, err
	}
	ips2, err := sd.resolveDNSBypass(ctx, host) // 绕过缓存，防止 DNS rebinding 漏过 TOCTOU
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "safe_dialer phase3 dns", err)
	}
	if sd.containsBlockedCIDR(ips2) {
		return nil, &SSRFBlockedError{Host: host, IPs: ips2}
	}

	// Phase 3.5: 响应 IP >20 拒绝
	if len(ips2) > 20 {
		return nil, &ErrDNSResponseTooLarge{Host: host, Count: len(ips2)}
	}

	// Phase 4: 依次尝试 ips2 中的 IP，实现类似标准库的 Happy Eyeballs 故障回退
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: sd.dialerControl, // 注入 Control 回调（local_only 时拒绝非 loopback）
	}
	var lastErr error
	for _, ip := range ips2 {
		target := net.JoinHostPort(ip.String(), port)
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "safe_dialer: all ips failed", lastErr)
	}
	return nil, perrors.New(perrors.CodeInternal, "safe_dialer: no ips to dial")
}

// dialerControl 在底层 socket 创建时调用。
// local_only 模式下由 NetworkSandbox 注入非 loopback 拒绝逻辑。
func (sd *SafeDialer) dialerControl(network, address string, c syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return nil // 无法解析，让后续 Dial 报错
	}

	// local_only 非 loopback 拒绝由 NetworkSandbox 通过 SetDialerControl 注入
	if sd.localOnlyIPFilter != nil {
		if !sd.localOnlyIPFilter(ip) {
			return &ErrNonLoopbackBlocked{IP: ip}
		}
	}

	return nil
}

// InjectHTTPTransport 将 SafeDialer 注入 http.Client DefaultTransport。
// 覆盖 REST/SSE 调用。
func (sd *SafeDialer) InjectHTTPTransport() {
	// 替换 http.DefaultTransport 的 DialContext
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}

	dt.DialContext = sd.DialContext

	// 禁用 HTTP/3 (QUIC): 移除 Alt-Svc 升级路径
	// http.Transport 默认不启用 QUIC，但显式设置 TLSClientConfig 确保
	if dt.TLSClientConfig == nil {
		dt.TLSClientConfig = &tls.Config{}
	}
	// 强制仅 HTTP/1.1 + HTTP/2，不升级到 HTTP/3
	dt.ForceAttemptHTTP2 = true
	dt.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"} // 显式排除 "h3"
}

// InjectWebSocketDialer 将 SafeDialer 注入 WebSocket 连接。
func (sd *SafeDialer) InjectWebSocketDialer(wsDialer interface {
	SetNetDialContext(func(context.Context, string, string) (net.Conn, error))
}) {
	wsDialer.SetNetDialContext(sd.DialContext)
}

// InjectGRPCDialer 将 SafeDialer 注入 gRPC 连接。
func (sd *SafeDialer) InjectGRPCDialer(opts interface {
	SetDialOption(func(context.Context, string) (net.Conn, error))
}) {
	opts.SetDialOption(func(ctx context.Context, addr string) (net.Conn, error) {
		return sd.DialContext(ctx, "tcp", addr)
	})
}

// SetLocalOnlyFilter 注入 local_only IP 过滤回调。
// 由 NetworkSandbox.Enable() 调用。
func (sd *SafeDialer) SetLocalOnlyFilter(filter func(net.IP) bool) {
	sd.localOnlyIPFilter = filter
}

// resolveDNS 解析 DNS（缓存 + TTL）。
func (sd *SafeDialer) resolveDNS(ctx context.Context, host string) ([]net.IP, error) {
	sd.dnsCacheMu.RLock()
	if ips, ok := sd.dnsCache[host]; ok {
		if time.Since(sd.dnsCacheTs[host]) < sd.dnsCacheTTL {
			sd.dnsCacheMu.RUnlock()
			return ips, nil
		}
	}
	sd.dnsCacheMu.RUnlock()
	return sd.resolveDNSBypass(ctx, host)
}

// resolveDNSBypass 强制绕过缓存执行真实 DNS 解析。
// Phase 3 TOCTOU 检查必须调用此方法，防止 DNS rebinding 漏过二次校验。
func (sd *SafeDialer) resolveDNSBypass(ctx context.Context, host string) ([]net.IP, error) {
	var r net.Resolver
	ips, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	result := make([]net.IP, len(ips))
	for i, ip := range ips {
		result[i] = ip.IP
	}

	// 写回缓存（更新时间戳）
	sd.dnsCacheMu.Lock()
	sd.dnsCache[host] = result
	sd.dnsCacheTs[host] = time.Now()
	sd.dnsCacheMu.Unlock()

	return result, nil
}

// containsBlockedCIDR 检查 IP 列表是否命中预编译的 parsedBlockedCIDRs。
// 无需持锁，parsedBlockedCIDRs 初始化后只读。
func (sd *SafeDialer) containsBlockedCIDR(ips []net.IP) bool {
	for _, ip := range ips {
		for _, block := range parsedBlockedCIDRs {
			if block.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// TaintEgressCheck 出口拦截: payload 中任一字段 TaintLevel ≥ TaintMedium
// 且未经 SanitizeByUserReview → 拒绝出站。
func (sd *SafeDialer) TaintEgressCheck(taintLevels []protocol.TaintLevel) error {
	for _, tl := range taintLevels {
		if tl >= protocol.TaintMedium {
			return &ErrTaintBlockedEgress{Level: tl}
		}
	}
	return nil
}

// Capability 出口强制检查。
type dialerCapability int

const (
	capReadOnly     dialerCapability = iota // 仅 GET/HEAD
	capWriteLocal                           // 仅内网 POST/PUT
	capWriteNetwork                         // 全网络
)

// CheckCapability Phase 0: Capability Token 出口强制。
func CheckCapability(cap dialerCapability, method string) error {
	switch cap {
	case capReadOnly:
		if !isReadOnlyHTTP(method) {
			return &ErrCapabilityWriteBlocked{Method: method}
		}
	case capWriteLocal:
		// 调用方负责在 DialContext 中校验 IP 为内网地址
	case capWriteNetwork:
		// 放行，后续 Phase 1-4 保护
	}
	return nil
}

func isReadOnlyHTTP(method string) bool {
	m := strings.ToUpper(method)
	return m == "GET" || m == "HEAD" || m == "OPTIONS"
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

type ErrTaintBlockedEgress struct {
	Level protocol.TaintLevel
}

func (e *ErrTaintBlockedEgress) Error() string {
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
