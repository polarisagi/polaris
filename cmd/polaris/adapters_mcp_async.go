package main

import (
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/pkg/types"
)

// mcpAsyncTaskAdapter 将 *mcp.MCPManager 适配为 get_task_result.AsyncTaskProvider
// （GD-08-001）。internal/tool/builtin 属 L1，internal/extension/mcp 属 L2，
// inv_NoCrossLayerImport 禁止 L1 反向 import L2；main 包不受层级限制，在此桥接。
type mcpAsyncTaskAdapter struct {
	inner *mcp.MCPManager
}

func (a *mcpAsyncTaskAdapter) GetAsyncTaskResult(taskID string) (status, text, errMsg string, images []types.ImagePart, found bool) {
	result, ok := a.inner.GetAsyncTaskResult(taskID)
	if !ok {
		return "", "", "", nil, false
	}
	return string(result.Status), result.Text, result.Error, result.Images, true
}
