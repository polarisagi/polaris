package action

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
)

// MockProxy 实现了 Dry-Run 模式下对工具请求的拦截与仿真响应。
// 现在作为真实的 HTTP 代理服务器运行。
type MockProxy struct {
	db     *sql.DB
	server *http.Server
	addr   string
}

func NewMockProxy(db *sql.DB) *MockProxy {
	return &MockProxy{db: db}
}

// Start 启动 HTTP 代理服务器，监听随机可用端口。
func (m *MockProxy) Start() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	m.addr = listener.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handleProxyRequest)

	m.server = &http.Server{Handler: mux}
	go func() {
		_ = m.server.Serve(listener)
	}()

	slog.Info("MockProxy HTTP server started", "addr", m.addr)
	return nil
}

// Stop 停止代理服务器
func (m *MockProxy) Stop(ctx context.Context) error {
	if m.server != nil {
		return m.server.Shutdown(ctx)
	}
	return nil
}

// Addr 返回代理监听的地址 (例如 127.0.0.1:xxx)
func (m *MockProxy) Addr() string {
	return m.addr
}

func (m *MockProxy) handleProxyRequest(w http.ResponseWriter, r *http.Request) {
	// 读取前1KB内容用于哈希
	bodyBytes := make([]byte, 1024)
	n, _ := io.ReadFull(r.Body, bodyBytes)
	bodyBytes = bodyBytes[:n]

	hashInput := r.Method + r.URL.String() + string(bodyBytes)
	hash := sha256.Sum256([]byte(hashInput))
	opHash := hex.EncodeToString(hash[:])[:32]

	var mockBody string
	var statusCode int

	err := m.db.QueryRow(`
		SELECT response_body, status_code FROM mock_response_cache
		WHERE operation_hash = ? LIMIT 1
	`, opHash).Scan(&mockBody, &statusCode)

	if err != nil {
		// Cache miss
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error": "Mock miss"}`))
		return
	}

	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(mockBody))
}

// Execute 拦截内部直接调用的工具调用，作为后备
func (m *MockProxy) Execute(ctx context.Context, toolName string, args []byte) (*protocol.ToolResult, error) {
	outMap := make(map[string]interface{})
	outMap["mocked"] = true
	outMap["tool"] = toolName

	bytes, _ := json.Marshal(outMap)
	return &protocol.ToolResult{
		Success: true,
		Output:  bytes,
	}, nil
}
