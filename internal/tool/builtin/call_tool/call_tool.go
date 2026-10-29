// Package call_tool 是常驻元工具 call_tool 的直调兜底（ADR-0120 决策三）。
//
// 模型发出的 call_tool 由内核在 S_VALIDATE 之前解包为内层调用（internal/agent/agent_call_tool.go），
// 此后的校验、审批、污点与执行都以内层工具名进行。本函数只在 call_tool 绕过内核被直接执行时触发
// （例如 code_act 里 polaris.call("call_tool", ...)），一律拒绝：转发会让内层调用跳过内核的
// 校验链，而 code_act 本就可以直接 polaris.call 任意工具。
package call_tool

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// CallToolFn 直调即拒绝。
func CallToolFn(_ context.Context, _ []byte) ([]byte, error) {
	return nil, apperr.New(apperr.CodeInvalidInput,
		"call_tool is resolved by the agent loop; inside code_act call the target tool directly with polaris.call(name, ...)")
}
