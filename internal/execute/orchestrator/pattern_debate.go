package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// DebateExecutor 实现了对抗性辩论/互审编排模式（编排模式11，GD-6）。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3-sexies
// 决策记录: docs/arch/decisions/ADR-0080-pattern-debate.md
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
//nolint:gocyclo,nestif,funlen
func (de *DebateExecutor) Execute(ctx context.Context, parentTaskID string, proponent, opponent, judge types.TaskEntry, maxRounds int) (verdict []byte, err error) {
	stateID := "debate-state"

	chk, err := de.chkRepo.GetCheckpoint(ctx, parentTaskID, stateID, 1)
	var state debateState
	if err != nil {
		if apperr.IsCode(err, apperr.CodeNotFound) {
			state = debateState{
				Round:          1,
				Phase:          "judge_init",
				CurrentSpeaker: "judge",
				NextSpeaker:    "proponent",
			}
		} else {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to load debate checkpoint", err)
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
				_ = de.saveCheckpoint(ctx, parentTaskID, stateID, state, "done")
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
