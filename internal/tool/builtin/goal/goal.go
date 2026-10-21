// Package goal 实现跨回合目标工具（ADR-0120 决策七）：目标落库，回合收尾时由网关自动续跑。
package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const maxObjectiveRunes = 2000

type args struct {
	Action       string `json:"action"`
	Objective    string `json:"objective"`
	DoneCriteria string `json:"done_criteria"`
	MaxRounds    int    `json:"max_rounds"`
	Status       string `json:"status"`
	Note         string `json:"note"`
}

// MakeGoalFn 构造 goal 工具执行函数；会话 ID 取自 ctx，缺失即报错（不回落共享存储）。
func MakeGoalFn(goals repo.GoalRepository) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if goals == nil {
			return nil, apperr.New(apperr.CodeInternal, "goal: 未注入 GoalRepository")
		}
		sessionID, err := guard.SessionIDFromCtx(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "goal", err)
		}
		var a args
		if err := json.Unmarshal(input, &a); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "goal: invalid args", err)
		}
		switch a.Action {
		case "set":
			return doSet(ctx, goals, sessionID, a)
		case "update":
			return doUpdate(ctx, goals, sessionID, a)
		case "get":
			return doGet(ctx, goals, sessionID)
		case "clear":
			if err := goals.PauseGoal(ctx, sessionID); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "goal: clear", err)
			}
			return []byte("goal paused"), nil
		default:
			return nil, apperr.New(apperr.CodeInvalidInput, "goal: action must be set|update|get|clear")
		}
	}
}

func doSet(ctx context.Context, goals repo.GoalRepository, sessionID string, a args) ([]byte, error) {
	obj := strings.TrimSpace(a.Objective)
	if obj == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "goal: objective is required for set")
	}
	if r := []rune(obj); len(r) > maxObjectiveRunes {
		obj = string(r[:maxObjectiveRunes])
	}
	g, err := goals.SetGoal(ctx, sessionID, obj, strings.TrimSpace(a.DoneCriteria), a.MaxRounds)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "goal: set", err)
	}
	return []byte(fmt.Sprintf("goal active (max %d continuation rounds). When finished call goal update status=complete; if stuck, status=blocked with a note.", g.MaxRounds)), nil
}

func doUpdate(ctx context.Context, goals repo.GoalRepository, sessionID string, a args) ([]byte, error) {
	switch a.Status {
	case "", repo.GoalActive, repo.GoalPaused, repo.GoalBlocked, repo.GoalComplete:
	default:
		return nil, apperr.New(apperr.CodeInvalidInput, "goal: status must be active|paused|blocked|complete")
	}
	if err := goals.UpdateGoal(ctx, sessionID, a.Status, strings.TrimSpace(a.Note)); err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "goal: update", err)
	}
	return []byte("goal updated"), nil
}

func doGet(ctx context.Context, goals repo.GoalRepository, sessionID string) ([]byte, error) {
	g, err := goals.CurrentGoal(ctx, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "goal: get", err)
	}
	if g == nil {
		return []byte("no goal"), nil
	}
	out := fmt.Sprintf("status=%s rounds=%d/%d objective=%s", g.Status, g.RoundsUsed, g.MaxRounds, g.Objective)
	if g.DoneCriteria != "" {
		out += "\ndone_criteria=" + g.DoneCriteria
	}
	if g.Note != "" {
		out += "\nnote=" + g.Note
	}
	return []byte(out), nil
}
