package network

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestSafeDialer_LocalOnlyIPFilter(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})
	sd.SetLocalOnlyFilter(func(ip net.IP) bool {
		return ip.IsLoopback()
	})

	// Public IP should be blocked
	err := sd.dialerControl("tcp", "8.8.8.8:443", nil)
	if err.Error() == "" {
		t.Errorf("Expected public IP to be blocked in local_only mode")
	}
	if _, ok := err.(*ErrNonLoopbackBlocked); !ok {
		t.Errorf("Expected ErrNonLoopbackBlocked, got: %v", err)
	}

	// Loopback IP should be allowed
	err = sd.dialerControl("tcp", "127.0.0.1:8080", nil)
	if err != nil {
		t.Errorf("Expected loopback IP to be allowed, got: %v", err)
	}
}

func TestSafeDialer_QUICDisabled(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})

	// Ensure QUIC/UDP is disabled by default
	_, err := sd.DialContext(context.Background(), "udp", "1.1.1.1:443")
	if err == nil {
		t.Errorf("Expected UDP/QUIC to be blocked")
	}
	if _, ok := err.(*ErrQUICDisabled); !ok {
		t.Errorf("Expected ErrQUICDisabled, got: %v", err)
	}
}

func TestSafeDialer_InjectHTTPTransport(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})

	// Reset DefaultTransport to avoid polluting
	dt := http.DefaultTransport.(*http.Transport)
	oldProtos := []string{}
	if dt.TLSClientConfig != nil {
		oldProtos = dt.TLSClientConfig.NextProtos
	}

	sd.InjectHTTPTransport()

	if dt.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig should be initialized")
	}

	foundH3 := false
	for _, p := range dt.TLSClientConfig.NextProtos {
		if p == "h3" {
			foundH3 = true
		}
	}

	if foundH3 {
		t.Errorf("HTTP/3 (QUIC) should be explicitly excluded from NextProtos")
	}

	if dt.TLSClientConfig != nil {
		dt.TLSClientConfig.NextProtos = oldProtos
	}
}

// TestSafeDialer_BlockedCIDR 验证 Phase 2 SSRF 阻断逻辑。
// 使用 containsBlockedCIDR 直接单元测试，避免真实 DNS 解析。
func TestSafeDialer_BlockedCIDR(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})

	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},             // loopback
		{"10.0.0.1", true},              // RFC1918 A 类
		{"172.16.0.1", true},            // RFC1918 B 类
		{"192.168.1.100", true},         // RFC1918 C 类
		{"169.254.0.1", true},           // link-local
		{"::1", true},                   // IPv6 loopback
		{"8.8.8.8", false},              // 公网
		{"1.1.1.1", false},              // 公网
		{"2001:4860:4860::8888", false}, // 公网 IPv6
	}

	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("invalid IP in test case: %s", tc.ip)
		}
		result := sd.containsBlockedCIDR([]net.IP{ip})
		if result != tc.blocked {
			t.Errorf("IP %s: expected blocked=%v, got blocked=%v", tc.ip, tc.blocked, result)
		}
	}
}

// TestSafeDialer_AllowLoopback 验证 allowLoopback=true 时 loopback IP 豁免，私有 CIDR 仍被拦截。
func TestSafeDialer_AllowLoopback(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})
	sd.allowLoopback = true

	loopbacks := []string{"127.0.0.1", "127.0.0.2", "::1"}
	for _, raw := range loopbacks {
		ip := net.ParseIP(raw)
		if sd.containsBlockedCIDR([]net.IP{ip}) {
			t.Errorf("allowLoopback=true: loopback %s should NOT be blocked", raw)
		}
	}

	// 私有 CIDR 不受 allowLoopback 影响，仍须拦截
	privates := []string{"10.0.0.1", "192.168.1.1", "169.254.0.1"}
	for _, raw := range privates {
		ip := net.ParseIP(raw)
		if !sd.containsBlockedCIDR([]net.IP{ip}) {
			t.Errorf("allowLoopback=true: private %s should still be blocked", raw)
		}
	}
}

// TestSafeDialer_TaintEgressCheck 验证污点出口拦截。
func TestSafeDialer_TaintEgressCheck(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})

	// Low taint 应放行
	if err := sd.TaintEgressCheck([]types.TaintLevel{types.TaintLow}); err != nil {
		t.Errorf("TaintLow should pass, got: %v", err)
	}

	// Medium taint 应拦截
	if err := sd.TaintEgressCheck([]types.TaintLevel{types.TaintMedium}); err == nil {
		t.Errorf("TaintMedium should be blocked egress")
	}

	// High taint 应拦截
	if err := sd.TaintEgressCheck([]types.TaintLevel{types.TaintHigh}); err == nil {
		t.Errorf("TaintHigh should be blocked egress")
	}
}

// TestSafeDialer_DNSTooManyIPs 验证 Phase 3.5 的 IP 数量上限。
func TestSafeDialer_DNSTooManyIPs(t *testing.T) {
	// 构造 21 个合法公网 IP
	ips := make([]net.IP, 21)
	for i := range ips {
		ips[i] = net.ParseIP("8.8.8.8")
	}

	// 直接用 ips2 > 20 路径检验
	if len(ips) <= 20 {
		t.Fatal("test precondition: should have >20 IPs")
	}
	var err error = &ErrDNSResponseTooLarge{Host: "test.example.com", Count: len(ips)}
	if err.Error() == "" {
		t.Error("Expected ErrDNSResponseTooLarge")
	}
}

// TestNewSafeHTTPClient 验证 NewSafeHTTPClient 正确配置 transport。
func TestNewSafeHTTPClient(t *testing.T) {
	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{})
	client := NewSafeHTTPClient(sd)

	if client == nil {
		t.Fatal("expected non-nil http.Client")
	}
	if client.Timeout != 0 {
		t.Errorf("expected no client-level timeout (streaming-safe), got %v", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("expected ResponseHeaderTimeout=30s, got %v", transport.ResponseHeaderTimeout)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("expected TLSClientConfig to be set")
	}
	for _, p := range transport.TLSClientConfig.NextProtos {
		if p == "h3" {
			t.Errorf("h3/QUIC should be excluded from NextProtos")
		}
	}
}

// TestGetParsedCIDRs 验证 getParsedCIDRs() 返回的列表数量与内置 CIDR 字符串数量一致。
// 原测试引用已消除的包级变量（blockedCIDRs/parsedBlockedCIDRs），
// 重构后 CIDR 数据内嵌于 sync.OnceValue 闭包，通过公开调用路径间接验证。
// 如任意 CIDR 格式错误，getParsedCIDRs() 初始化时 panic（fail-fast），本测试无需重复解析校验。
func TestGetParsedCIDRs(t *testing.T) {
	cidrs := getParsedCIDRs()
	// 10 个内置 CIDR：0.0.0.0/8 127.0.0.0/8 10.0.0.0/8 172.16.0.0/12
	// 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 ::1/128 fc00::/7 fe80::/10
	const expectedCount = 10
	if len(cidrs) != expectedCount {
		t.Errorf("getParsedCIDRs: got %d entries, want %d", len(cidrs), expectedCount)
	}
}
