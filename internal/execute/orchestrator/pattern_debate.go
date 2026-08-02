package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// DebateExecutor 实现了对抗性辩论/互审编排模式（编排模式11，GD-6）。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3-sexies
// 决策记录: docs/arch/decisions/ADR-0046-execute-module.md（决策四，含原 ADR-0080）
type DebateExecutor struct {
	bb      *SQLiteBlackboard
	chkRepo protocol.TaskCheckpointRepository
}

// NewDebateExecutor 创建 DebateExecutor。
func NewDebateExecutor(bb *SQLiteBlackboard) *DebateExecutor {
	return &DebateExecutor{
		bb:      bb,
		chkRepo: repo.NewSQLiteTaskCheckpointRepository(bb.DB()),
	}
}

// debateState 承载 Debate 执行的检查点状态。
type debateState struct {
	Round          int      `json:"round"`
	CurrentSpeaker string   `json:"current_speaker"`
	NextSpeaker    string   `json:"next_speaker"`
	History        []string `json:"history"`
	Phase          string   `json:"phase"` // "judge_init", "debate", "judge_final"
	InFlightTask   string   `json:"in_flight_task"`
	JudgeFinalized bool     `json:"judge_finalized"`
	Verdict        string   `json:"verdict"`
}

// Execute 执行三方辩论：Judge 初始议题 -> Proponent/Opponent 轮番辩论 -> Judge 结案陈词。
// 遵循 GD-6 约束，本模式内部各任务间等待复用 checkpoint 异步挂起原语，不阻塞轮询。
//
// 已知缺口（诚实声明，非本次范围）：本函数用返回 apperr "suspend" 错误表示
// "尚未完成，需要在子任务结束后被重新调用"，但目前系统内没有任何组件会在
// 子任务完成时自动重新调用 Execute——本轮修复只接线了 boot_agent.go 的
// DebateExecutor 构造与 AgentBundle 注入，未接入任何触发重调用的编排循环
// （与 internal/agent 侧 transfer_to_agent 曾经的同类问题一致，见 GD-1）。
// 调用方在真正把本模式接入生产调度前，必须先设计等价于 GD-1
// watchHandoffCompletion 的重调用驱动，否则辩论会在首次挂起后停滞。
// 单元测试（pattern_debate_test.go）仅验证状态机本身的 checkpoint 往返
// 正确性，不代表已具备生产可用的自动恢复能力。
//
//nolint:gocyclo,nestif,funlen
func (de *DebateExecutor) Execute(ctx context.Context, parentTaskID string, proponent, opponent, judge types.TaskEntry, maxRounds int) (verdict []byte, err error) {
	stateID := "debate-state"

	// [修复] protocol.TaskCheckpointRepository.GetCheckpoint 对"未找到"的约定是
	// 返回 (nil, nil)，不是 (nil, CodeNotFound) 错误——与 pattern_state_graph.go
	// checkNodeCheckpoint 的既有用法一致。此前代码误判为后者，导致每次全新
	// 辩论的首次调用都会在 chk 为 nil 时对 chk.OutputJSON 解引用而 panic
	// （100% 复现，见 pattern_debate_test.go 回归测试）。
	chk, err := de.chkRepo.GetCheckpoint(ctx, parentTaskID, stateID, 1)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to load debate checkpoint", err)
	}
	var state debateState
	if chk == nil {
		state = debateState{
			Round:          1,
			Phase:          "judge_init",
			CurrentSpeaker: "judge",
			NextSpeaker:    "proponent",
		}
	} else {
		if err := json.Unmarshal([]byte(chk.OutputJSON), &state); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to unmarshal debate state", err)
		}
	}

	if state.JudgeFinalized {
		return []byte(state.Verdict), nil
	}

	// [GD-1] 异步挂起唤醒后，检查飞行中的任务是否完成
	if state.InFlightTask != "" {
		snap, err := de.bb.PeekTask(ctx, state.InFlightTask)
		if err == nil && snap != nil && snap.Status == types.TaskDone {
			// 收集任务结果
			state.History = append(state.History, fmt.Sprintf("[%s]: %s", state.CurrentSpeaker, string(snap.Result)))
			state.InFlightTask = ""

			switch state.Phase {
			case "judge_init":
				state.Phase = "debate"
				state.CurrentSpeaker = "proponent"
			case "debate":
				if state.CurrentSpeaker == "proponent" {
					state.CurrentSpeaker = "opponent"
				} else {
					state.CurrentSpeaker = "proponent"
					state.Round++
				}
				if state.Round > maxRounds {
					state.Phase = "judge_final"
					state.CurrentSpeaker = "judge"
				}
			case "judge_final":
				state.JudgeFinalized = true
				state.Verdict = string(snap.Result)
				// checkpoint 写入是尽力而为（不阻断裁决结果返回），但持续失败意味着
				// 崩溃恢复无法从"已终裁"状态续跑（会被误判为仍需重新辩论），值得观测
				// （2026-08-02 补齐，见 99-new-findings.md 阶段02 §2.5 发现）。
				if err := de.saveCheckpoint(ctx, parentTaskID, stateID, state, "done"); err != nil {
					metrics.GlobalCheckpointWriteFailuresTotal.Add(1)
					slog.Error("debate: 终裁 checkpoint 写入失败，崩溃恢复可能从错误状态续跑",
						"task_id", parentTaskID, "state_id", stateID, "err", err)
				}
				return []byte(state.Verdict), nil
			}
		} else if err == nil && snap != nil && snap.Status == types.TaskFailed {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("debate task %s failed", snap.ID))
		} else {
			// 还在执行中，继续挂起
			return nil, apperr.New(apperr.CodeInternal, "suspend")
		}
	}

	// 发起新任务
	var target types.TaskEntry
	switch state.CurrentSpeaker {
	case "judge":
		target = judge
	case "proponent":
		target = proponent
	case "opponent":
		target = opponent
	}

	// 拼接上下文意图
	contextIntent := string(target.Intent)
	if len(state.History) > 0 {
		contextIntent += "\n\n[Debate History]:\n"
		for _, h := range state.History {
			contextIntent += h + "\n"
		}
	}

	target.ID = fmt.Sprintf("debate-%s-%d-%s", parentTaskID, state.Round, state.CurrentSpeaker)
	if target.Type == "" {
		target.Type = "agent_handoff:" + state.CurrentSpeaker
	}
	target.Intent = []byte(contextIntent)

	if err := de.bb.PostTask(ctx, &target); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to post debate task", err)
	}

	state.InFlightTask = target.ID
	if err := de.saveCheckpoint(ctx, parentTaskID, stateID, state, "executing"); err != nil {
		slog.Warn("debate: failed to save checkpoint", "err", err)
	}

	// 返回挂起
	return nil, apperr.New(apperr.CodeInternal, "suspend")
}

func (de *DebateExecutor) saveCheckpoint(ctx context.Context, taskID, nodeID string, state debateState, status string) error {
	out, _ := json.Marshal(state)
	chk := types.TaskCheckpointRow{
		TaskID:     taskID,
		NodeID:     nodeID,
		Attempt:    1,
		Status:     status,
		OutputJSON: string(out),
		TaintLevel: 0,
	}
	if err := de.chkRepo.UpsertCheckpoint(ctx, chk); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "upsert debate checkpoint", err)
	}
	return nil
}
