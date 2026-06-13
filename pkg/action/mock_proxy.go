package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"sync"
)

// MockProxy 是 MCTS 试运行期间的本地 HTTP 代理服务器。
type MockProxy struct {
	mockTable map[string]MockResponse
	listener  net.Listener
	mu        sync.RWMutex
	server    *http.Server
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
		return nil, "", err
	}
	mp := &MockProxy{mockTable: mockTable, listener: ln}
	mp.server = &http.Server{Handler: mp}
	go func() {
		_ = mp.server.Serve(ln)
	}()
	return mp, ln.Addr().String(), nil
}

// ServeHTTP 实现 http.Handler 接口，处理所有代理请求。
func (mp *MockProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// EnvVars 返回需要注入沙箱的环境变量。
func (mp *MockProxy) EnvVars() map[string]string {
	addr := mp.listener.Addr().String()
	return map[string]string{
		"HTTP_PROXY":  "http://" + addr,
		"HTTPS_PROXY": "http://" + addr,
		"NO_PROXY":    "",
	}
}

// Close 优雅关闭代理服务器。
func (mp *MockProxy) Close() error {
	if mp.server != nil {
		return mp.server.Shutdown(context.Background())
	}
	return nil
}
