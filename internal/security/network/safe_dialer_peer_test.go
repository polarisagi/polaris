package network

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

// TestSafeDialer_PeerAllowlistIsExactHostPort ADR-0111 决策六第 2 条负向用例：
// 对端放行只认登记过的精确 host:port；同主机其他端口、未登记的私网 / CGNAT 地址仍被拦截。
// 用回环监听模拟一个「本属私网段、默认被 SSRF 防护拦下」的对端。
func TestSafeDialer_PeerAllowlistIsExactHostPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	sd := NewSafeDialer(0, nil, config.M11PolicyThresholds{SafeDialerTOCTOUDelayMs: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var ssrf *SSRFBlockedError
	if _, err := sd.DialContext(ctx, "tcp", ln.Addr().String()); !errors.As(err, &ssrf) {
		t.Fatalf("without allowlist the private address must be blocked, got %v", err)
	}

	sd.SetPeerAllowlist(func(host string, p int) bool { return host == "127.0.0.1" && p == port })
	conn, err := sd.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("registered peer host:port must be reachable, got %v", err)
	}
	_ = conn.Close()

	other := net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1))
	if _, err := sd.DialContext(ctx, "tcp", other); !errors.As(err, &ssrf) {
		t.Fatalf("another port on the same host must stay blocked, got %v", err)
	}
	if _, err := sd.DialContext(ctx, "tcp", "100.100.100.100:28888"); !errors.As(err, &ssrf) {
		t.Fatalf("unregistered CGNAT (Tailscale range) address must stay blocked, got %v", err)
	}
}
