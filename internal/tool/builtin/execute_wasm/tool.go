package execute_wasm

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func MakeExecuteWasmFn(allowedPaths []string) sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var args struct {
			Code      string `json:"code"`
			Input     string `json:"input"`
			Network   bool   `json:"network_allowed"`
			MaxPages  int    `json:"max_pages"`
			Workspace string `json:"workspace"`
			// TimeoutMs 墙钟超时预算（毫秒）；<=0 时 WasmtimeExecute 使用默认值
			// 5000ms（Batch11 GR-7.1，此前该 FFI 调用完全没有超时预算传参）。
			TimeoutMs int `json:"timeout_ms"`
		}
		if err := json.Unmarshal(spec.Input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid json", err)
		}

		cleanWorkspace := filepath.Clean(args.Workspace)
		if !guard.IsPathAllowed(cleanWorkspace, allowedPaths) {
			return nil, apperr.New(apperr.CodeInternal, "workspace path not allowed")
		}

		quota := toolsb.CalculateWasmQuota(spec.SystemTier, spec.TaintLevel)
		if args.MaxPages > 0 && args.MaxPages < quota.MemoryPages {
			quota.MemoryPages = args.MaxPages
		}

		// 这里实际依赖 toolsb.WasmtimeExecute FFI，如果是在纯 Go 层我们假设其内部处理了隔离
		outJSON, err := toolsb.WasmtimeExecute(
			ctx,
			[]byte(args.Code),
			args.Input,
			cleanWorkspace,
			quota.MemoryPages,
			args.Network,
			quota.Fuel,
			10*1024*1024,
			args.TimeoutMs,
		)

		if err != nil {
			//nolint:nilerr
			return &types.ToolResult{
				Success: false,
				Error:   err.Error(),
			}, nil
		}

		return &types.ToolResult{
			Success: true,
			Output:  []byte(outJSON),
		}, nil
	}
}
