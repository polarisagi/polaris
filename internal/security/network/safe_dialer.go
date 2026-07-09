package network

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// getParsedCIDRs 返回预编译的内网 CIDR 阻断列表（惰性初始化，只读，无需加锁）。
// 架构文档: docs/arch/M11-Policy-Safety.md §6
// 使用 sync.OnceValue 而非包级 var + init()：避免包初始化顺序依赖，并消除全局可变状态。
var getParsedCIDRs = sync.OnceValue(func() []*net.IPNet {
	cidrs := []string{
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
	blocks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("safe_dialer: invalid built-in CIDR " + cidr + ": " + err.Error())
		}
		blocks = append(blocks, block)
	}
	return blocks
})

// SafeDialer 统一安全拨号器 —— SSRFGuard 五阶段校验的唯一实现。
// 实现 internal/protocol/interfaces.go (SafeDialer)。
// 所有出站网络连接必须通过此入口，CI safe_dialer_lint 扫描裸 net.Dial/grpc.Dial/http.Get → ERROR。
type SafeDialer struct {
	dnsCache    map[string][]net.IP // hostname → resolved IPs
	dnsCacheTTL time.Duration       // 从 config 注入
	dnsCacheMu  sync.RWMutex
	dnsCacheTs  map[string]time.Time

	// QUIC/HTTP3 已禁用 — 禁止 UDP 绕过 DialContext。
	// Go net/http 默认不启用 QUIC；quic-go 通过 dialer.Control 在 SafeDialer 中显式拒绝 UDP。
	quicDisabled bool

	// localOnlyIPFilter local_only 模式下的 IP 过滤函数。
	// nil = 未启用；非 nil = 仅允许 filter(ip)==true 的 IP。
	localOnlyIPFilter func(net.IP) bool

	// allowLoopback 允许连接 loopback 地址（127.0.0.0/8 与 ::1）。
	// 仅用于系统级受控本地服务（如 Ollama），跳过该 CIDR 段的 SSRF 阻断。
	// 不影响其余私有 CIDR（10.x / 172.16.x / 192.168.x 等）仍被拦截。
	allowLoopback bool

	taintLevel     int
	allowedDomains []string

	toctouDelayMs   int
	maxIPsThreshold int
}

var _ protocol.SafeDialer = (*SafeDialer)(nil)

// NewSafeDialer 初始化安全拨号器。
func NewSafeDialer(taintLevel int, allowedDomains []string, m11 config.M11PolicyThresholds) *SafeDialer {
	ttl := 30 * time.Second
	if m11.SafeDialerDNSCacheTTLSecond > 0 {
		ttl = time.Duration(m11.SafeDialerDNSCacheTTLSecond) * time.Second
	}
	delay := 50
	if m11.SafeDialerTOCTOUDelayMs > 0 {
		delay = m11.SafeDialerTOCTOUDelayMs
	}
	maxIPs := 20
	if m11.SafeDialerMaxIPsThreshold > 0 {
		maxIPs = m11.SafeDialerMaxIPsThreshold
	}

	return &SafeDialer{
		dnsCache:        make(map[string][]net.IP),
		dnsCacheTTL:     ttl,
		dnsCacheTs:      make(map[string]time.Time),
		quicDisabled:    true, // 禁用 QUIC/HTTP3 — 防止 UDP 绕过 DialContext
		taintLevel:      taintLevel,
		allowedDomains:  allowedDomains,
		toctouDelayMs:   delay,
		maxIPsThreshold: maxIPs,
	}
}

// NewSafeHTTPClient 返回一个绑定了 SafeDialer 的 *http.Client。
// 所有 Adapter 和工具调用须使用此工厂，禁止传入 http.DefaultClient。
func NewSafeHTTPClient(sd *SafeDialer) *http.Client {
	if sd == nil {
		sd = NewSafeDialer(0, nil, config.M11PolicyThresholds{})
	}
	return newSafeHTTPClientFromDialer(sd)
}

// NewLoopbackSafeHTTPClient 返回允许连接 loopback（127.x / ::1）的安全 HTTP 客户端。
// 仅用于系统级受控本地服务（Ollama inference / embedding / QLoRA / PRM / Steering）。
// 其余私有 CIDR 仍受 SafeDialer SSRF 阻断保护。
func NewLoopbackSafeHTTPClient(m11 config.M11PolicyThresholds) *http.Client {
	sd := NewSafeDialer(0, nil, m11)
	sd.allowLoopback = true
	c := newSafeHTTPClientFromDialer(sd)
	if t, ok := c.Transport.(*http.Transport); ok {
		t.ResponseHeaderTimeout = 300 * time.Second
	}
	return c
}

// newSafeHTTPClientFromDialer 从已配置的 SafeDialer 构造 http.Client（内部共用逻辑）。
func newSafeHTTPClientFromDialer(sd *SafeDialer) *http.Client {
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
func (sd *SafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) { //nolint:gocyclo,nestif
	// QUIC/HTTP3 阻断: 拒绝 UDP 连接
	if sd.quicDisabled && strings.EqualFold(network, "udp") {
		return nil, &ErrQUICDisabled{}
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = address
		port = "443"
	}

	if types.TaintLevel(sd.taintLevel) == types.TaintMedium {
		allowed := false
		for _, d := range sd.allowedDomains {
			if strings.EqualFold(host, d) || strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(d)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, apperr.New(apperr.CodeForbidden, "safe_dialer: TaintMedium requests are restricted to allowed domains")
		}
	}

	// Phase 1: DNS 解析
	ips1, err := sd.resolveDNS(ctx, host)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "safe_dialer phase1 dns", err)
	}

	// Phase 2: blockedCIDRs 校验
	if sd.containsBlockedCIDR(ips1) {
		return nil, &SSRFBlockedError{Host: host, IPs: ips1}
	}

	// Phase 3: 50ms TOCTOU 延迟 + 二次 DNS 解析（强制绕过缓存，TOCTOU 保护）
	if err := sleepCtx(ctx, time.Duration(sd.toctouDelayMs)*time.Millisecond); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SafeDialer.DialContext", err)
	}
	ips2, err := sd.resolveDNSBypass(ctx, host) // 绕过缓存，防止 DNS rebinding 漏过 TOCTOU
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "safe_dialer phase3 dns", err)
	}
	if sd.containsBlockedCIDR(ips2) {
		return nil, &SSRFBlockedError{Host: host, IPs: ips2}
	}

	// Phase 3.5: 响应 IP >20 拒绝
	if len(ips2) > sd.maxIPsThreshold {
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
		return nil, apperr.Wrap(apperr.CodeInternal, "safe_dialer: all ips failed", lastErr)
	}
	return nil, apperr.New(apperr.CodeInternal, "safe_dialer: no ips to dial")
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
		return nil, apperr.Wrap(apperr.CodeInternal, "SafeDialer.resolveDNSBypass", err)
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

// containsBlockedCIDR 检查 IP 列表是否命中内网 CIDR 阻断列表。
// getParsedCIDRs() 内部通过 sync.OnceValue 保证只初始化一次，调用后只读无锁。
// 当 sd.allowLoopback=true 时，loopback IP（127.x.x.x / ::1）跳过 CIDR 检查——
// 仅适用于系统级受控本地服务（Ollama），不影响其余私有 CIDR 的拦截。
func (sd *SafeDialer) containsBlockedCIDR(ips []net.IP) bool {
	for _, ip := range ips {
		if sd.allowLoopback && ip.IsLoopback() {
			continue // 系统级本地服务豁免，跳过此 IP
		}
		for _, block := range getParsedCIDRs() {
			if block.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// TaintEgressCheck / Capability 检查(CheckCapability) / ValidateGitURL / 错误类型定义
// 见 safe_dialer_capability.go（R7 拆分）。
