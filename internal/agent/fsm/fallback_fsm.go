package fsm

import (
	"context"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// FallbackFSM 是确定性状态机的一种实现变体——零外部依赖，适用于测试和降级路径。
// 权威状态枚举定义见 internal/protocol/types.go (AgentState, AgentTrigger)。
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §1

// FallbackFSM 零外部依赖的确定性状态机。
type FallbackFSM struct {
	state          types.AgentState
	transitions    map[types.AgentState]map[types.AgentTrigger]types.AgentState
	callbacks      map[types.AgentState]func(ctx context.Context) error
	stateDeadlines map[types.AgentState]time.Duration
	replanCount    int
}

// NewFallbackFSM 创建带默认死区时间的后备状态机。
func NewFallbackFSM(initial types.AgentState) *FallbackFSM {
	return &FallbackFSM{
		state:          initial,
		transitions:    make(map[types.AgentState]map[types.AgentTrigger]types.AgentState),
		callbacks:      make(map[types.AgentState]func(ctx context.Context) error),
		stateDeadlines: make(map[types.AgentState]time.Duration),
		replanCount:    0,
	}
}

// AddDeadline 添加状态截止时间。
func (fsm *FallbackFSM) AddDeadline(state types.AgentState, deadline time.Duration) {
	fsm.stateDeadlines[state] = deadline
}

// GetDeadline 获取状态截止时间。
func (fsm *FallbackFSM) GetDeadline(state types.AgentState) time.Duration {
	return fsm.stateDeadlines[state]
}

// Transition 执行状态转移。
// 覆盖全部 ReplanGuard 路径: S_VALIDATE 失败 / S_ROLLBACK 完成 /
// M1 FatalStreamAbort / JSON Repair 失败 / S_PLAN 拓扑失败。
func (fsm *FallbackFSM) Transition(ctx context.Context, trigger types.AgentTrigger) error {
	toState, ok := fsm.transitions[fsm.state][trigger]
	if !ok {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("invalid transition: state=%v trigger=%v", fsm.state, trigger))
	}

	if toState == types.AgentStateReplan {
		fsm.replanCount++
		if fsm.replanCount > 3 {
			toState = types.AgentStateFailed
		}
	}

	fsm.state = toState

	if cb, ok := fsm.callbacks[toState]; ok {
		return cb(ctx)
	}
	return nil
}
