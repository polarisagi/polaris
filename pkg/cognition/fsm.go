package cognition

import (
	"context"
	"fmt"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// FallbackFSM 是确定性状态机的一种实现变体——零外部依赖，适用于测试和降级路径。
// 权威状态枚举定义见 internal/protocol/types.go (AgentState, AgentTrigger)。
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §1

// FallbackFSM 零外部依赖的确定性状态机。
type FallbackFSM struct {
	state          protocol.AgentState
	transitions    map[protocol.AgentState]map[protocol.AgentTrigger]protocol.AgentState
	callbacks      map[protocol.AgentState]func(ctx context.Context) error
	stateDeadlines map[protocol.AgentState]time.Duration
	replanCount    int
}

// NewFallbackFSM 创建带默认死区时间的后备状态机。
func NewFallbackFSM(initial protocol.AgentState) *FallbackFSM {
	return &FallbackFSM{
		state:          initial,
		transitions:    make(map[protocol.AgentState]map[protocol.AgentTrigger]protocol.AgentState),
		callbacks:      make(map[protocol.AgentState]func(ctx context.Context) error),
		stateDeadlines: make(map[protocol.AgentState]time.Duration),
		replanCount:    0,
	}
}

// AddDeadline 添加状态截止时间。
func (fsm *FallbackFSM) AddDeadline(state protocol.AgentState, deadline time.Duration) {
	fsm.stateDeadlines[state] = deadline
}

// GetDeadline 获取状态截止时间。
func (fsm *FallbackFSM) GetDeadline(state protocol.AgentState) time.Duration {
	return fsm.stateDeadlines[state]
}

// Transition 执行状态转移。
// 覆盖全部 ReplanGuard 路径: S_VALIDATE 失败 / S_ROLLBACK 完成 /
// M1 FatalStreamAbort / JSON Repair 失败 / S_PLAN 拓扑失败。
func (fsm *FallbackFSM) Transition(ctx context.Context, trigger protocol.AgentTrigger) error {
	toState, ok := fsm.transitions[fsm.state][trigger]
	if !ok {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("invalid transition: state=%v trigger=%v", fsm.state, trigger))
	}

	if toState == protocol.AgentStateReplan {
		fsm.replanCount++
		if fsm.replanCount > 3 {
			toState = protocol.AgentStateFailed
		}
	}

	fsm.state = toState

	if cb, ok := fsm.callbacks[toState]; ok {
		return cb(ctx)
	}
	return nil
}
