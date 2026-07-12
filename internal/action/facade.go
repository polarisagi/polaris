package action

import (
	"context"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ActionFacadeImpl 包装 *codeact.CodeAct 提供统一门面（R1.4：消费端接口
// `gateway/server/provider.go` 的 `CodeActEngine` 已定义所需方法集，本结构体
// 通过 Go 结构化类型隐式满足，不在生产方重复声明接口）。
//
// 设计原则：
//   - 上层模块（gateway/server）依赖自己声明的 CodeActEngine 接口，不直接持有
//     *codeact.CodeAct 具体 struct
//   - codeact.CodeAct 内部 AST 检查/L1 治理/L2 LLM 评审/HITL 网关对调用方透明
//
// @producer: ActionFacadeImpl（由 cmd/polaris/boot_server.go 构造并注入）
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
