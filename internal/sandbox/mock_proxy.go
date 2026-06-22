package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// MockProxy 是 MCTS 试运行期间的本地 HTTP 代理服务器。
type MockProxy struct {
	mockTable  map[string]MockResponse
	listener   net.Listener
	mu         sync.RWMutex
	server     *http.Server
	caKey      *ecdsa.PrivateKey // 自签根 CA 私钥
	caCert     *x509.Certificate // 根 CA 证书（DER 解析后）
	caCertPEM  []byte            // 根 CA 证书 PEM（注入沙箱 SSL_CERT_FILE）
	caCertFile string            // 临时文件路径（Close 时删除）
}

// MockResponse 单条 Mock 响应定义。
type MockResponse struct {
	StatusCode int               `json:"status_code"` // 默认 200
	Body       json.RawMessage   `json:"body"`        // 响应体 JSON
	Headers    map[string]string `json:"headers"`     // 可选响应头
}

// NewMockProxy 创建并启动 MockProxy，监听 127.0.0.1:0（动态端口）。
// 返回 *MockProxy 和代理监听地址（如 "127.0.0.1:54321"）。
func NewMockProxy(mockTable map[string]MockResponse) (*MockProxy, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("NewMockProxy: %w", err)
	}
	mp := &MockProxy{mockTable: mockTable, listener: ln}

	// 生成自签根 CA（仅用于 DryRun 沙箱，生命周期与 MockProxy 相同）
	if err := mp.initCA(); err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("NewMockProxy: %w", err)
	}

	// 将 CA cert 写到临时文件（供沙箱 SSL_CERT_FILE 指向）
	f, err := os.CreateTemp("", "polaris-mock-ca-*.pem")
	if err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("NewMockProxy: %w", err)
	}
	if _, err := f.Write(mp.caCertPEM); err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("NewMockProxy: %w", err)
	}
	_ = f.Close()
	mp.caCertFile = f.Name()

	mp.server = &http.Server{Handler: mp}
	go func() { _ = mp.server.Serve(ln) }()
	return mp, ln.Addr().String(), nil
}

func (mp *MockProxy) initCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("MockProxy.initCA: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Polaris MockProxy CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("MockProxy.initCA: %w", err)
	}
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return fmt.Errorf("MockProxy.initCA: %w", err)
	}
	mp.caKey = key
	mp.caCert = cert
	mp.caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	return nil
}

// ServeHTTP 实现 http.Handler 接口，处理所有代理请求。
func (mp *MockProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// HTTPS 隧道请求：劫持连接，MITM 解密后查 mockTable
	if r.Method == http.MethodConnect {
		mp.handleConnect(w, r)
		return
	}

	hashInput := r.Method + " " + r.URL.String()
	hash := sha256.Sum256([]byte(hashInput))
	opHash := hex.EncodeToString(hash[:])[:16]

	mp.mu.RLock()
	resp, ok := mp.mockTable[opHash]
	mp.mu.RUnlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"mocked": true}`))
		return
	}

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}

	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	} else {
		_, _ = w.Write([]byte(`{}`))
	}
}

// handleConnect 处理 HTTP CONNECT 请求，实现 TLS MITM 拦截。
// 流程：响应 200 Connection established → 劫持 TCP 连接 → 用叶子证书做 TLS 握手
// → 以明文 HTTP 读取内部请求 → 查 mockTable → 响应。
func (mp *MockProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	// 为目标域名生成叶子证书（用根 CA 签名）
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		http.Error(w, "cert gen failed", http.StatusInternalServerError)
		return
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, mp.caCert, &leafKey.PublicKey, mp.caKey)
	if err != nil {
		http.Error(w, "leaf cert failed", http.StatusInternalServerError)
		return
	}
	tlsCert := tls.Certificate{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafKey,
	}

	// 劫持底层 TCP 连接
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}

	// 回应 CONNECT 200（标准 HTTP 代理协议要求）
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

	// TLS 握手
	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	})
	defer tlsConn.Close()
	if err := tlsConn.Handshake(); err != nil {
		return
	}

	// 以明文读取内部 HTTP 请求，复用 ServeHTTP 逻辑
	innerReq, err := http.ReadRequest(bufio.NewReader(tlsConn))
	if err != nil {
		return
	}
	// 修正 URL（CONNECT 隧道内的请求 URL 是相对路径）
	if innerReq.URL.Host == "" {
		innerReq.URL.Host = r.Host
		innerReq.URL.Scheme = "https"
	}

	// 构造 ResponseWriter 写回 tlsConn
	rw := &connResponseWriter{conn: tlsConn}
	mp.ServeHTTP(rw, innerReq)
	_ = rw.flush()
}

// connResponseWriter 将 http.ResponseWriter 接口桥接到裸 net.Conn。
type connResponseWriter struct {
	conn    net.Conn
	headers http.Header
	status  int
	body    []byte
}

func (c *connResponseWriter) Header() http.Header {
	if c.headers == nil {
		c.headers = make(http.Header)
	}
	return c.headers
}
func (c *connResponseWriter) WriteHeader(status int) { c.status = status }
func (c *connResponseWriter) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return len(b), nil
}
func (c *connResponseWriter) flush() error {
	if c.status == 0 {
		c.status = 200
	}
	resp := &http.Response{
		StatusCode: c.status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1, ProtoMinor: 1,
		Header:        c.headers,
		Body:          io.NopCloser(bytes.NewReader(c.body)),
		ContentLength: int64(len(c.body)),
	}
	return resp.Write(c.conn)
}

// EnvVars 返回需要注入沙箱的环境变量。
func (mp *MockProxy) EnvVars() map[string]string {
	addr := mp.listener.Addr().String()
	env := map[string]string{
		"HTTP_PROXY":  "http://" + addr,
		"HTTPS_PROXY": "http://" + addr,
		"NO_PROXY":    "",
	}
	if mp.caCertFile != "" {
		// Python requests、curl、Go net/http 均读取这两个环境变量
		env["SSL_CERT_FILE"] = mp.caCertFile
		env["REQUESTS_CA_BUNDLE"] = mp.caCertFile
		// Node.js
		env["NODE_EXTRA_CA_CERTS"] = mp.caCertFile
	}
	return env
}

// Close 优雅关闭代理服务器。
func (mp *MockProxy) Close() error {
	var errs []error
	if mp.server != nil {
		if err := mp.server.Shutdown(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}
	if mp.caCertFile != "" {
		_ = os.Remove(mp.caCertFile)
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
