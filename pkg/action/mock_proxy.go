package action

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
)

// MockProxy 实现了 Dry-Run 模式下对工具请求的拦截与仿真响应。
// 依据 TaintLevel，如果是 DryRun 模式，则从 032_mock_response_cache.sql 中查询对应的 Mock 响应返回。
type MockProxy struct {
	db *sql.DB
}

func NewMockProxy(db *sql.DB) *MockProxy {
	return &MockProxy{db: db}
}

// Execute 拦截工具调用
func (m *MockProxy) Execute(ctx context.Context, toolName string, args []byte) (*protocol.ToolResult, error) {
	// 如果需要实现 Dry-Run 拦截逻辑：
	// 返回预设好的 JSON 等
	// 这里做个简单的实现
	outMap := make(map[string]interface{})
	outMap["mocked"] = true
	outMap["tool"] = toolName

	bytes, _ := json.Marshal(outMap)
	return &protocol.ToolResult{
		Success: true,
		Output:  bytes,
	}, nil
}
