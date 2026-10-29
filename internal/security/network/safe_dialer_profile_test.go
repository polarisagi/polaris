package network_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/network"
)

func TestSafeDialer_AuditOnly_BypassesSSRFBlock(t *testing.T) {
	// 启动一个仅供连接测试的本地 TCP 监听（127.0.0.1 属于私有环回地址）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen on loopback:", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	// 1. 标准 SafeDialer（未开启 allowLoopback 且未开启 auditOnly）尝试拨号 127.0.0.1 → 必须被 SSRF 阻断
	sdNormal := network.NewSafeDialer(0, nil, config.M11PolicyThresholds{
		SafeDialerTOCTOUDelayMs: 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = sdNormal.DialContext(ctx, "tcp", addr)
	if err == nil {
		t.Fatal("normal safe dialer should block connection to loopback IP (127.0.0.1)")
	}

	// 2. auditOnly SafeDialer（full 信任档位）尝试拨号 127.0.0.1 → 绕过 SSRF 阻断，成功建立连接
	sdAudit := network.NewSafeDialer(0, nil, config.M11PolicyThresholds{
		SafeDialerTOCTOUDelayMs: 1,
	}).WithAuditOnly(true)

	conn, err := sdAudit.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("audit-only safe dialer should not block connection: %v", err)
	}
	_ = conn.Close()
}
