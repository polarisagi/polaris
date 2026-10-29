package agent

import "github.com/polarisagi/polaris/pkg/types"

// appendLoopMessages 是向当前回合追加消息的唯一入口（R3-06 / R4-06 下沉到 fsm.StateContext）。
func (a *Agent) appendLoopMessages(msgs ...types.Message) {
	if a == nil || a.sCtx == nil || len(msgs) == 0 {
		return
	}
	a.sCtx.AppendLoopMessages(msgs...)
}
