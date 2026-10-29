package meshapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
)

// AgentCardPath 公开 Agent Card（免鉴权，A2A 1.0 well-known 路径）。
const AgentCardPath = "/.well-known/agent-card.json"

// PeerAllowedPath 对端令牌（ClientTypePeer）可访问的路由白名单（ADR-0111 决策三第 2 条）。
// 精确匹配，不做前缀：/mcpx、/a2a/v1/../_admin 之类的变体一律不放行。
func PeerAllowedPath(path string) bool {
	return path == A2APath || path == MCPPath || path == AgentCardPath
}

// IsMeshPath 两个门面的路径。中间件据此把它们排除在「静态外壳免鉴权」之外——那条规则
// 豁免任意非 /v1、非 /_admin 的 GET，不排除的话 GET /mcp 会被当成静态页放过。
func IsMeshPath(path string) bool {
	return path == A2APath || path == MCPPath || strings.HasPrefix(path, A2APath+"/") || strings.HasPrefix(path, MCPPath+"/")
}

// Deps 门面依赖。
type Deps struct {
	Service *mesh.Service
	A2A     config.A2AConfig
	// BlockingWait A2A SendMessage 未带 returnImmediately 时的最长阻塞时间。
	BlockingWait time.Duration
}

// Register 挂载 Agent Card、A2A 1.0 与 MCP 门面。
func Register(mux *http.ServeMux, d Deps) {
	if d.BlockingWait <= 0 {
		d.BlockingWait = 60 * time.Second
	}
	mux.HandleFunc("GET "+AgentCardPath, cardHandler(d.A2A, d.Service.Self()))
	mux.Handle(A2APath, a2asrv.NewJSONRPCHandler(&a2aHandler{svc: d.Service, blockingWait: d.BlockingWait}))
	mux.Handle(MCPPath, &mcpServer{svc: d.Service, agentName: d.A2A.Name})
}
