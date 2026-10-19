package agent

import "github.com/polarisagi/polaris/pkg/types"

// appendLoopMessages 是向当前回合追加消息的唯一入口（R3-06）。
// 加锁保护并统一为未设置轮次的消息打上当前 LoopRound，避免持久化遗漏。
func (a *Agent) appendLoopMessages(msgs ...types.Message) {
	if a == nil || a.sCtx == nil || len(msgs) == 0 {
		return
	}
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()

	round := a.sCtx.LoopRound
	for i := range msgs {
		if msgs[i].Round <= 0 {
			msgs[i].Round = round
		}
	}
	a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, msgs...)
}
