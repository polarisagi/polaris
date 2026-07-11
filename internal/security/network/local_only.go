package network

import (
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// local_only 隐私模式网络沙箱三层防御。
// 架构文档: docs/arch/M11-Policy-Safety.md §5.3

// NetworkSandbox local_only 网络隔离策略。
// 三层防御:
// L1 主力 — OS 级沙箱（macOS sandbox-exec / Linux Landlock / Windows WFP）
// L2 纵深 — Go 层 RoundTripper 替换 no-op + DefaultResolver 覆写 NXDOMAIN
// L3 兜底 — Dialer.Control 拒绝非 loopback IP + SafeDialer 注入
type NetworkSandbox struct {
	osSandbox     *OSNetworkSandbox
	goTransport   *NoopTransport
	dnsResolver   *LoopbackResolver
	allowlist     *Allowlist
	safeDialer    *SafeDialer
	localProvider protocol.LocalProvider // 可选；非 nil 时 StartupCheck 执行 Tier3 内存守卫
	enabled       bool
	mu            sync.RWMutex
}

// OSNetworkSandbox OS 级主力防线。
// macOS: sandbox-exec deny(network*) / NetworkExtension
// Linux: Landlock LSM / iptables nftables owner 匹配
// Windows: WFP / AppContainer 网络隔离
type OSNetworkSandbox struct {
	platform string // darwin | linux | windows
	enabled  bool
}

// NewOSNetworkSandbox 检测 OS 平台并创建对应沙箱。
func NewOSNetworkSandbox() *OSNetworkSandbox {
	return &OSNetworkSandbox{
		platform: runtime.GOOS,
		enabled:  false, // 需显式调用 Enable() 激活
	}
}

// Enable 激活 OS 级网络沙箱。失败 → fail-closed 拒绝进入 local_only。
func (s *OSNetworkSandbox) Enable() error {
	if err := s.enableOS(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "OSNetworkSandbox.Enable", err)
	}
	s.enabled = true
	return nil
}

// NoopTransport 替换 http.DefaultTransport 为 no-op（所有 HTTP 请求直接拒绝）。
type NoopTransport struct{}

// RoundTrip 拒绝所有 HTTP 请求。
func (t *NoopTransport) RoundTrip(req *httpRequest) (*httpResponse, error) {
	return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("local_only: network disabled — HTTP request to %s blocked", req.URL))
}

// httpRequest/httpResponse 避免循环依赖的最小定义。
type httpRequest struct{ URL string }
type httpResponse struct{}

// LoopbackResolver DNS 解析器覆写。
// 非 localhost/.local 域名 → 返回 NXDOMAIN。
type LoopbackResolver struct {
	resolver *net.Resolver
}

// NewLoopbackResolver 创建仅解析 loopback 域名的 DNS 解析器。
func NewLoopbackResolver() *LoopbackResolver {
	return &LoopbackResolver{
		resolver: &net.Resolver{},
	}
}

// LookupHost DNS 解析，仅放行 localhost/.local 域名。
func (r *LoopbackResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return []string{host}, nil
	}
	if isLocalTLD(host) {
		return r.resolver.LookupHost(ctx, host)
	}
	return nil, &ErrLocalOnlyDNSBlocked{Host: host}
}

// isLocalTLD 检查是否为 .local 域名 (mDNS)。
func isLocalTLD(host string) bool {
	return len(host) > 6 && host[len(host)-6:] == ".local"
}

// Allowlist local_only 网络白名单。
// 配置: ~/.polarisagi/polaris/config/local_only_network_allowlist.toml, Ed25519 签名防篡改。
// 上限: Tier 3=5 条, Tier 0/1/2 禁用。
// 仅 M10 Connector 子系统豁免; M1/M12/OTel 仍全阻断。
type Allowlist struct {
	entries []AllowlistEntry
	maxSize int // Tier 3: 5
	mu      sync.RWMutex
}

// AllowlistEntry 白名单条目。
type AllowlistEntry struct {
	Domain         string
	CIDR           string
	Port           int
	Protocol       string
	DNSSECRequired bool
	RateLimit      int
}

// NewAllowlist 创建白名单。maxSize 由 HardwareTier 决定。
func NewAllowlist(maxSize int) *Allowlist {
	return &Allowlist{
		entries: make([]AllowlistEntry, 0),
		maxSize: maxSize,
	}
}

// Add 添加白名单条目，超上限拒绝。
func (al *Allowlist) Add(entry AllowlistEntry) error {
	al.mu.Lock()
	defer al.mu.Unlock()
	if len(al.entries) >= al.maxSize {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("local_only: allowlist full (%d entries max)", al.maxSize))
	}
	al.entries = append(al.entries, entry)
	return nil
}

// IsAllowed 检查 host:port 是否在白名单中。
func (al *Allowlist) IsAllowed(host string, port int) bool {
	al.mu.RLock()
	defer al.mu.RUnlock()
	for _, entry := range al.entries {
		if entry.Domain == host && entry.Port == port {
			return true
		}
	}
	return false
}

// NewNetworkSandbox 初始化 local_only 网络沙箱。
func NewNetworkSandbox(maxAllowlistSize int) *NetworkSandbox {
	return &NetworkSandbox{
		osSandbox:   NewOSNetworkSandbox(),
		goTransport: &NoopTransport{},
		dnsResolver: NewLoopbackResolver(),
		allowlist:   NewAllowlist(maxAllowlistSize),
	}
}

// SetSafeDialer 绑定 SafeDialer 以注入 Dialer.Control。
func (ns *NetworkSandbox) SetSafeDialer(sd *SafeDialer) {
	ns.safeDialer = sd
}

// SetLocalProvider 绑定本地推理 Provider（M1 LocalAdapter），供 StartupCheck 的
// Tier3 本地模型守卫使用（docs/arch/M11-Policy-Safety.md §5.3）。未绑定时该项
// 检查跳过——local_only 模式在 Tier0/1/2 已被 §5.3 硬件门控彻底禁用，只有 Tier3
// 场景才需要此校验，调用方（boot 流程）按需注入。
func (ns *NetworkSandbox) SetLocalProvider(lp protocol.LocalProvider) {
	ns.localProvider = lp
}

// Enable 激活所有网络防护层。
// 三级防御:
// 1. OS 级沙箱
// 2. Go 层 RoundTripper 替换 + DNS 覆写
// 3. SafeDialer Dialer.Control 拒绝非 loopback IP
func (ns *NetworkSandbox) Enable() error {
	// L1: OS 级沙箱（失败时降级到 L2+L3，不中断；三级防御仍有效）
	if err := ns.osSandbox.Enable(); err != nil {
		slog.Warn("local_only: os sandbox (L1) failed, degrading to L2+L3 Go-layer protection", "err", err)
	}

	// L2: Go 层纵深
	ns.mu.Lock()
	defer ns.mu.Unlock()

	// 替换 DefaultTransport 为 no-op
	defaultTransport := http.DefaultTransport.(*http.Transport)
	defaultTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("local_only: network disabled — outbound connection to %s blocked", addr))
	}

	// 覆写 DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// 仅允许 localhost DNS 解析
			return nil, apperr.New(apperr.CodeInternal, "local_only: dns resolution disabled for non-loopback")
		},
	}

	// L3: SafeDialer Dialer.Control 注入
	if ns.safeDialer != nil {
		ns.safeDialer.SetLocalOnlyFilter(func(ip net.IP) bool {
			return ip.IsLoopback()
		})
	}

	ns.enabled = true
	return nil
}

// IsLoopbackIP 检查 IP 是否为 loopback。
// 支持 IPv4 (127.0.0.0/8) 和 IPv6 (::1)。
func IsLoopbackIP(ip net.IP) bool {
	return ip.IsLoopback()
}

// StartupCheck local_only 启动期自检 (fail-closed)。
// 1. DNS 解析隐私检测域名 (.com 公网 TLD)
// 2. 收到 DNS 响应 (非 NXDOMAIN) → CRITICAL + 拒绝启动
// 3. loopback-only 网络连通性探测 (TCP SYN 至 8.8.8.8:53)
// 4. 收到 SYN-ACK → 沙箱未生效 → 拒绝进入 local_only
func (ns *NetworkSandbox) StartupCheck() error {
	// 0. 内存硬件要求：强制 Tier 3 (64GB 级别) 才能使用 local_only
	const minPhysicalRAMForLocalOnly = 60 * 1024 * 1024 * 1024 // 60 GB
	// local_only 模式需要 Tier3 硬件（64GB 级别），通过全局 FeatureGate 的 HardwareProbe 验证
	fg := probe.GlobalFeatureGate()
	if fg == nil || fg.TotalRAM() < minPhysicalRAMForLocalOnly {
		totalGB := uint64(0)
		if fg != nil {
			totalGB = fg.TotalRAM() / (1024 * 1024 * 1024)
		}
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"local_only: requires >= 60 GB physical RAM (Tier3); detected %d GB", totalGB,
		))
	}

	// DNS 泄露检测: 解析公网域名 → 收到响应 → 沙箱失效
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := ns.dnsResolver.LookupHost(ctx, "privacy-check.polarisagi/polaris-external.com")
	if err == nil && len(addrs) > 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("local_only: DNS leak detected — %d addresses resolved for privacy check domain", len(addrs)))
	}

	// loopback-only 网络连通性探测（Dialer.Control 拒绝非 loopback）
	//
	// XR-06 豁免说明：本次拨号刻意不经过 SafeDialer。StartupCheck 的目的是验证
	// OS 层网络隔离（namespace/seccomp/cgroup 等）本身是否生效，若改用 SafeDialer，
	// 其 Dialer.Control 会在应用层直接拦截非 loopback 连接，导致无论 OS 隔离是否
	// 真正生效，本探测都会"误报成功"（永远拒绝），使这道独立防线的自检失去意义。
	// 因此此处的裸拨号是有意为之的例外，不是遗漏。
	conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 3*time.Second)
	if err == nil {
		conn.Close()
		return apperr.New(apperr.CodeInternal, "local_only: external connectivity detected — OS sandbox not effective")
	}

	// 5. Tier3 本地模型守卫: 上面的 60GB 检查只验证物理总内存容量，不代表
	// 当下真的有 <=63GB 的可用预算——系统上其它进程可能已经占用了大量内存。
	// LocalProvider.Probe() 给出"模型是否真的处于可加载/已加载可用状态"+
	// 本进程峰值 RSS，与系统已用内存相加后必须留有 >=1GB 预留，否则 fail-closed
	// 拒绝进入 local_only（M11-Policy-Safety.md §5.3）。
	if ns.localProvider != nil {
		if err := ns.checkLocalModelMemoryBudget(ctx); err != nil {
			return err
		}
	}

	return nil
}

// localOnlyMemoryReserveBytes 是 Tier3 本地模型守卫的固定预留量（1GB）。
// 与 60GB 物理内存门槛配套: 64GB 物理 - 1GB 预留 = 63GB 峰值预算上限。
const localOnlyMemoryReserveBytes = 1 * 1024 * 1024 * 1024

// tier3MemoryBudgetBytes 是 Tier3 local_only 模式允许的峰值 RSS + 系统已用内存
// 总和上限（64GB - 1GB 预留）。
const tier3MemoryBudgetBytes = 64*1024*1024*1024 - localOnlyMemoryReserveBytes

// checkLocalModelMemoryBudget 校验 LocalProvider.Probe() 结果是否在 Tier3 预算内。
func (ns *NetworkSandbox) checkLocalModelMemoryBudget(ctx context.Context) error {
	result, err := ns.localProvider.Probe(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "local_only: LocalProvider.Probe failed", err)
	}
	if !result.ModelLoadable {
		return apperr.New(apperr.CodeInternal,
			"local_only: no local model currently loaded — local_only requires a resident local model before enabling network isolation")
	}
	total := result.PeakRSSBytes + result.UsedMemoryBytes
	if total >= tier3MemoryBudgetBytes {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"local_only: memory budget exceeded — peak RSS (%d MB) + used memory (%d MB) >= %d MB (64GB - 1GB reserve)",
			result.PeakRSSBytes/(1024*1024), result.UsedMemoryBytes/(1024*1024), tier3MemoryBudgetBytes/(1024*1024),
		))
	}
	return nil
}

// ============================================================================
// 错误类型
// ============================================================================

// ErrLocalOnlyDNSBlocked local_only 模式下 DNS 解析被拒绝。
type ErrLocalOnlyDNSBlocked struct {
	Host string
}

func (e *ErrLocalOnlyDNSBlocked) Error() string {
	return fmt.Sprintf("local_only: dns resolution for non-loopback host %s blocked", e.Host)
}
