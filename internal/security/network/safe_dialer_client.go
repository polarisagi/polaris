package network

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

// SafeHTTPClient 包装了 *http.Client 并标记为已受 SafeDialer 保护
type SafeHTTPClient struct {
	*http.Client
	isSafe bool
}

// IsSafe 返回该客户端是否真正经过安全配置
func (s SafeHTTPClient) IsSafe() bool {
	return s.isSafe
}

// NewSafeHTTPClient 返回一个绑定了 SafeDialer 的 SafeHTTPClient。
// 所有 Adapter 和工具调用须使用此工厂，禁止传入 http.DefaultClient。
func NewSafeHTTPClient(sd *SafeDialer) SafeHTTPClient {
	if sd == nil {
		sd = NewSafeDialer(0, nil, config.M11PolicyThresholds{})
	}
	return SafeHTTPClient{Client: newSafeHTTPClientFromDialer(sd), isSafe: true}
}

// NewLoopbackSafeHTTPClient 返回允许连接 loopback（127.x / ::1）的安全 HTTP 客户端。
// 仅用于系统级受控本地服务（Ollama inference / embedding / QLoRA / PRM / Steering）。
// 其余私有 CIDR 仍受 SafeDialer SSRF 阻断保护。
func NewLoopbackSafeHTTPClient(m11 config.M11PolicyThresholds) SafeHTTPClient {
	sd := NewSafeDialer(0, nil, m11)
	sd.allowLoopback = true
	c := newSafeHTTPClientFromDialer(sd)
	if t, ok := c.Transport.(*http.Transport); ok {
		t.ResponseHeaderTimeout = 300 * time.Second
	}
	return SafeHTTPClient{Client: c, isSafe: true}
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
