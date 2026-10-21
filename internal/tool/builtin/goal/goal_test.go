package goal

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
)

type fakeGoals struct{ g *repo.Goal }

func (f *fakeGoals) SetGoal(_ context.Context, sid, obj, done string, max int) (repo.Goal, error) {
	if max <= 0 {
		max = 20
	}
	f.g = &repo.Goal{ID: "g1", SessionID: sid, Objective: obj, DoneCriteria: done, Status: repo.GoalActive, MaxRounds: max}
	return *f.g, nil
}
func (f *fakeGoals) CurrentGoal(context.Context, string) (*repo.Goal, error) { return f.g, nil }
func (f *fakeGoals) ActiveGoal(context.Context, string) (*repo.Goal, error)  { return f.g, nil }
func (f *fakeGoals) UpdateGoal(_ context.Context, _, status, note string) error {
	if status != "" {
		f.g.Status = status
	}
	f.g.Note = note
	return nil
}
func (f *fakeGoals) BeginRound(context.Context, string) (*repo.Goal, error) { return f.g, nil }
func (f *fakeGoals) FinishRound(context.Context, string, bool, int) (*repo.Goal, error) {
	return f.g, nil
}
func (f *fakeGoals) PauseGoal(context.Context, string) error {
	f.g.Status = repo.GoalPaused
	return nil
}

func call(t *testing.T, fn func(context.Context, []byte) ([]byte, error), ctx context.Context, in string) string {
	t.Helper()
	out, err := fn(ctx, []byte(in))
	if err != nil {
		t.Fatalf("%s: %v", in, err)
	}
	return string(out)
}

func TestGoalTool_Lifecycle(t *testing.T) {
	f := &fakeGoals{}
	fn := MakeGoalFn(f)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "s1")
	if got := call(t, fn, ctx, `{"action":"set","objective":"迁移数据库","done_criteria":"测试全绿","max_rounds":5}`); !strings.Contains(got, "max 5") {
		t.Fatal(got)
	}
	if got := call(t, fn, ctx, `{"action":"get"}`); !strings.Contains(got, "迁移数据库") || !strings.Contains(got, "测试全绿") {
		t.Fatal(got)
	}
	call(t, fn, ctx, `{"action":"update","status":"complete","note":"done"}`)
	if f.g.Status != repo.GoalComplete {
		t.Fatalf("status=%s", f.g.Status)
	}
	call(t, fn, ctx, `{"action":"clear"}`)
	if f.g.Status != repo.GoalPaused {
		t.Fatalf("status=%s", f.g.Status)
	}
}

func TestGoalTool_Errors(t *testing.T) {
	fn := MakeGoalFn(&fakeGoals{})
	if _, err := fn(context.Background(), []byte(`{"action":"get"}`)); err == nil {
		t.Fatal("缺会话 ID 应报错")
	}
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "s1")
	for _, in := range []string{`{"action":"set"}`, `{"action":"bogus"}`, `{"action":"update","status":"x"}`} {
		if _, err := fn(ctx, []byte(in)); err == nil {
			t.Fatalf("%s 应报错", in)
		}
	}
	if _, err := MakeGoalFn(nil)(ctx, []byte(`{"action":"get"}`)); err == nil {
		t.Fatal("nil repo 应报错")
	}
}
