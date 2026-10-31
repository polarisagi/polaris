package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// goalMaxIdleRounds 连续这么多个续跑回合没有任何工具调用即自动暂停目标（ADR-0120 决策七）。
const goalMaxIdleRounds = 2

// GoalStore 目标续跑所需的窄接口（HE-3：由 repo.GoalRepository 满足）。
type GoalStore interface {
	ActiveGoal(ctx context.Context, sessionID string) (*repo.Goal, error)
	BeginRound(ctx context.Context, goalID string) (*repo.Goal, error)
	FinishRound(ctx context.Context, goalID string, idle bool, maxIdle int) (*repo.Goal, error)
	PauseGoal(ctx context.Context, sessionID string) error
}

// goalContinuationInput 续跑回合的内部指令。文本由模型设定的目标拼成，按 TaintHigh 送入。
func goalContinuationInput(g *repo.Goal) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<goal-continue>\n继续推进目标：%s（第 %d/%d 轮）\n", g.Objective, g.RoundsUsed, g.MaxRounds)
	if g.DoneCriteria != "" {
		sb.WriteString("完成标准：" + g.DoneCriteria + "\n")
	}
	sb.WriteString("已完成请调用 goal{action:update,status:complete}；无法继续请 status:blocked 并写明原因。\n</goal-continue>")
	return sb.String()
}

// continueOnGoal 回合正常结束且会话存在 active 目标时自动发起续跑回合（ADR-0120 决策七）。
// 续跑发生在同一回合内：用户点停止即中止当前续跑并暂停目标；用户插话沿用既有的回合内 steer。
// 停止条件：目标不再 active、轮次耗尽、连续 goalMaxIdleRounds 个续跑回合零工具调用、回合出错/被中止。
func (o *orchestrator) continueOnGoal(
	ctx context.Context, sink Sink, sessionID string, agentCtrl protocol.AgentController,
	history []types.Message, reply string, rec *stepRecorder,
) (string, string, bool, []string) {
	var allViewIDs []string
	if o.goals == nil {
		return reply, "", false, allViewIDs
	}
	for {
		g, err := o.goals.ActiveGoal(ctx, sessionID)
		if err != nil || g == nil || ctx.Err() != nil {
			return reply, "", false, allViewIDs
		}
		if g.RoundsUsed >= g.MaxRounds {
			slog.Info("session: goal rounds exhausted, pausing", "session", sessionID, "goal", g.ID)
			o.pauseGoal(ctx, sessionID)
			_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{
				"type": "goal_paused", "message": "目标续跑轮次已用完，已暂停", "rounds": g.RoundsUsed, "max_rounds": g.MaxRounds}})
			return reply, "", false, allViewIDs
		}
		g, err = o.goals.BeginRound(ctx, g.ID)
		if err != nil || g == nil {
			return reply, "", false, allViewIDs
		}
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, sErr := o.persistence.SaveMessage(saveCtx, sessionID, "assistant", reply, "", "", 0); sErr != nil {
			slog.Error("session: saveMessage assistant before goal continuation", "session", sessionID, "error_meta", safeErrorSummary(sErr))
		}
		cancel()
		history = append(history, types.Message{Role: "assistant", Content: reply})
		agentCtrl.SetConversationHistory(o.withTurnDigests(ctx, sessionID, history))
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{
			"type": "goal_continue", "message": "继续推进目标：" + g.Objective,
			"round": g.RoundsUsed, "max_rounds": g.MaxRounds, "source": "goal"}})
		slog.Info("session: goal continuation", "session", sessionID, "goal", g.ID, "round", g.RoundsUsed)

		before := rec.toolCount()
		next, inferErr, _, aborted, viewIDs := o.runFSMTurn(ctx, sink, sessionID, agentCtrl,
			goalContinuationInput(g), types.TaintHigh)
		allViewIDs = append(allViewIDs, viewIDs...)
		if aborted {
			o.pauseGoal(context.Background(), sessionID) // 用户停止优先于续跑
			return next, inferErr, true, allViewIDs
		}
		if inferErr != "" || next == "" {
			o.pauseGoal(ctx, sessionID)
			return next, inferErr, false, allViewIDs
		}
		if _, fErr := o.goals.FinishRound(ctx, g.ID, rec.toolCount() == before, goalMaxIdleRounds); fErr != nil {
			slog.Warn("session: goal finish round", "session", sessionID, "error_meta", safeErrorSummary(fErr))
		}
		reply = next
	}
}

// pauseGoal 暂停 active 目标；失败只记日志（目标最坏多续跑一轮，不阻断回合收尾）。
func (o *orchestrator) pauseGoal(ctx context.Context, sessionID string) {
	if err := o.goals.PauseGoal(ctx, sessionID); err != nil {
		slog.Warn("session: pause goal failed", "session", sessionID, "error_meta", safeErrorSummary(err))
	}
}
