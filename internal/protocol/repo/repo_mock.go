package repo

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// MockResponseCache 定义 032_mock_response_cache 表的持久化层接口
// @consumer ShadowExecutor
type MockResponseCache interface {
	// GetMockResponse 根据 operation_hash 获取 mock 响应，未命中返回 apperr.CodeNotFound
	GetMockResponse(ctx context.Context, operationHash string) (*protocol.MockResponse, error)

	// SaveMockResponse 存入 mock 响应
	SaveMockResponse(ctx context.Context, resp *protocol.MockResponse) error
}
