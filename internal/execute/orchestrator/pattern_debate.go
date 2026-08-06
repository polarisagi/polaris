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
// 挂起/续跑约定：本函数用返回 apperr "suspend" 错误表示"尚未完成，需要在
// 子任务结束后被重新调用"。该重调用驱动由 DebateWorker 提供（debate_worker.go）
// ——它认领 type=="debate" 的任务，监听 task_completed/task_failed 后重调
// Execute，实现断点续跑；boot_agent.go 构造并启动它。
//
// 注：此处原有一段"已知缺口：无任何组件会自动重新调用 Execute，辩论会在首次
// 挂起后停滞"的声明。该缺口已由 DebateWorker 补齐，注释于 2026-08-06 订正
// ——留着会让维护者误以为本模式至今不可用。
//
// 无 Saga 补偿是**刻意的**，不是遗漏：辩论子任务产出的是论点文本（正方/反方/
// 裁判的陈述），不产生需要回滚的外部副作用。为它加补偿等于给一个只读推理
// 流程套上事务语义，徒增复杂度。若将来辩论参与方被允许调用有副作用的工具，
// 需重新评估（届时应复用 StateGraphExecutor 的补偿协调，而非另写一份）。
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
