// Package get_task_result 实现 get_task_result 内置工具（GD-08-001，
// docs/arch/M13-bis-Extension-Registry.md §8.4）：供 LLM 轮询由 MCP 工具的
// *_async 变体（见 internal/extension/mcp.makeMCPToolAsyncFn）发起的异步任务。
package get_task_result

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type input struct {
	TaskID string `json:"task_id"`
}

// AsyncTaskProvider 是本工具所需的最小依赖接口（consumer-side 定义，HE-3）。
// internal/tool/builtin 属 L1，*mcp.MCPManager 属 L2（internal/extension/mcp），
// inv_NoCrossLayerImport 禁止 L1 反向 import L2；实现由 cmd/polaris 的
// adapter（main 包不受层级限制）包装 *mcp.MCPManager.GetAsyncTaskResult 提供，
// 返回值拆成基础类型，本包无需引用 mcp 包的任何具体类型。
type AsyncTaskProvider interface {
	GetAsyncTaskResult(taskID string) (status, text, errMsg string, images []types.ImagePart, taintLevel types.TaintLevel, found bool)
}

// MakeGetTaskResultFn 创建 get_task_result 工具执行函数。
// provider 为 nil 时（如未启用 MCP 的部署）始终返回 expired_or_not_found，
// 不 panic——与其余可选依赖工具的降级模式一致。
func MakeGetTaskResultFn(provider AsyncTaskProvider) sandbox.InProcessRichFn {
	return func(_ context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var in input
		if len(spec.Input) > 0 {
			if err := json.Unmarshal(spec.Input, &in); err != nil {
				return nil, apperr.New(apperr.CodeInvalidInput, "get_task_result: invalid input JSON: "+err.Error())
			}
		}
		if in.TaskID == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "get_task_result: task_id is required")
		}

		if provider == nil {
			return notFoundResult()
		}
		status, text, errMsg, images, taintLevel, found := provider.GetAsyncTaskResult(in.TaskID)
		if !found {
			return notFoundResult()
		}

		switch status {
		case "done":
			out, err := json.Marshal(map[string]string{"status": status, "result": text})
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "get_task_result: marshal done result", err)
			}
			// taintLevel 来自异步任务实际执行时 CallToolTainted 测得的污点等级，
			// 必须回传给 ToolResult.TaintLevel，否则 agent_execute_dag.go 的
			// GlobalTaintLevel 抬升逻辑会漏掉所有异步 MCP 工具结果（同步路径已在
			// makeMCPToolFn 修复，此处是其异步对应物）。
			return &types.ToolResult{Success: true, Output: out, ImageParts: images, TaintLevel: taintLevel}, nil
		case "failed":
			out, err := json.Marshal(map[string]string{"status": status, "error": errMsg})
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "get_task_result: marshal failed result", err)
			}
			return &types.ToolResult{Success: true, Output: out}, nil
		default: // pending
			out, err := json.Marshal(map[string]string{"status": status})
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "get_task_result: marshal pending result", err)
			}
			return &types.ToolResult{Success: true, Output: out}, nil
		}
	}
}

func notFoundResult() (*types.ToolResult, error) {
	out, err := json.Marshal(map[string]string{"status": "expired_or_not_found"})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "get_task_result: marshal not-found result", err)
	}
	return &types.ToolResult{Success: true, Output: out}, nil
}
