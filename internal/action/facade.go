package action

import (
	"context"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ActionFacade action 包对外统一接口（CodeAct 执行入口）。
//
// 设计原则：
//   - 上层模块（gateway/server）依赖此接口，不直接持有 *codeact.CodeAct 具体 struct
//   - codeact.CodeAct 内部 AST 检查/L1 治理/L2 LLM 评审/HITL 网关对调用方透明
//
// @consumer: gateway/server/handler_codeact.go
// @producer: ActionFacadeImpl（由 cmd/polaris/boot_server.go 构造并注入）
type ActionFacade interface {
	// ExecuteCode 同步执行 LLM 生成代码（强制 Sbx-L3 沙箱）。
	// 引擎未初始化时返回 apperr.CodeInternal 错误。
	ExecuteCode(ctx context.Context, req protocol.CodeActRequest) (*protocol.CodeActResult, error)
	// CodeActAvailable 返回 CodeAct 引擎是否已就绪（降级判断用）。
	CodeActAvailable() bool
}

// ActionFacadeImpl 包装 *codeact.CodeAct 提供统一门面。
type ActionFacadeImpl struct {
	ca *codeact.CodeAct
}

// NewActionFacade 构造 action 门面。ca 为 nil 时 CodeActAvailable 返回 false（降级拒绝）。
func NewActionFacade(ca *codeact.CodeAct) *ActionFacadeImpl {
	return &ActionFacadeImpl{ca: ca}
}

func (f *ActionFacadeImpl) ExecuteCode(ctx context.Context, req protocol.CodeActRequest) (*protocol.CodeActResult, error) {
	if f.ca == nil {
		return nil, apperr.New(apperr.CodeInternal, "action: codeact engine not initialized")
	}
	return f.ca.Execute(ctx, req)
}

func (f *ActionFacadeImpl) CodeActAvailable() bool {
	return f.ca != nil
}
