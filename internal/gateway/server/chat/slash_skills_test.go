package chat

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/pkg/types"
)

type listSkillRegistry struct{ skills []types.SkillMeta }

func (r listSkillRegistry) Register(context.Context, types.SkillMeta) error { return nil }
func (r listSkillRegistry) Get(_ context.Context, name, _ string) (*types.SkillMeta, error) {
	for i := range r.skills {
		if r.skills[i].Name == name {
			return &r.skills[i], nil
		}
	}
	return nil, nil
}
func (r listSkillRegistry) List(context.Context, types.SkillFilter) ([]types.SkillMeta, error) {
	return r.skills, nil
}
func (r listSkillRegistry) Deprecate(context.Context, string, string, string) error { return nil }

type echoSkillExec struct{}

func (echoSkillExec) ExecuteSkill(_ context.Context, id string, input []byte) ([]byte, error) {
	return []byte("rendered " + id + " " + string(input)), nil
}

func (echoSkillExec) ValidateSkill([]byte) error { return nil }

type nopSink struct{ text strings.Builder }

func (s *nopSink) Emit(e session.Event) error {
	s.text.WriteString(e.Text)
	return nil
}

func newSkillRouter() *SlashCommandRouter {
	r := NewSlashCommandRouter(nil, nil)
	r.SetSkills(listSkillRegistry{skills: []types.SkillMeta{
		{Name: "skill:deploy__ship", DisplayName: "deploy:ship", Description: "Ship it", Spec: `{"argument_hint":"[env]"}`},
		{Name: "skill:ops__ship", DisplayName: "ops:ship"},
		{Name: "skill:solo", DisplayName: "solo"},
		{Name: "skill:model_only", DisplayName: "model-only", DisableUserInvocation: true},
	}}, echoSkillExec{})
	return r
}

func TestSlashRouter_ExpandsUserSkill(t *testing.T) {
	r := newSkillRouter()
	ctx := context.Background()
	for input, wantSkill := range map[string]string{
		"/deploy:ship prod": "skill:deploy__ship",
		"$solo arg":         "skill:solo",
		"/solo":             "skill:solo",
	} {
		res := r.Dispatch(ctx, input, "s1", nil, nil, &nopSink{}, nil)
		if res.Handled || !strings.Contains(res.RewrittenInput, "rendered "+wantSkill) {
			t.Errorf("%q: handled=%v rewritten=%q", input, res.Handled, res.RewrittenInput)
		}
	}
	res := r.Dispatch(ctx, "/deploy:ship prod", "s1", nil, nil, &nopSink{}, nil)
	if !strings.Contains(res.RewrittenInput, `{"arguments":"prod"}`) || !strings.HasPrefix(res.RewrittenInput, "<command-name>/deploy:ship</command-name>") {
		t.Fatalf("arguments / command header not passed: %q", res.RewrittenInput)
	}
}

func TestSlashRouter_UserSkillNonMatches(t *testing.T) {
	r := newSkillRouter()
	ctx := context.Background()
	// 裸名歧义（deploy:ship / ops:ship）与仅模型可调用技能都不得被用户命令触发。
	for _, input := range []string{"/ship x", "/model-only", "$ship"} {
		res := r.Dispatch(ctx, input, "s1", nil, nil, &nopSink{}, nil)
		if res.RewrittenInput != "" {
			t.Errorf("%q must not expand: %q", input, res.RewrittenInput)
		}
	}
	// "$" 开头的普通消息（非技能名）照常进入推理，不被当作未知命令。
	if res := r.Dispatch(ctx, "$100 is the price", "s1", nil, nil, &nopSink{}, nil); res.Handled || res.RewrittenInput != "" {
		t.Fatalf("plain $ message must pass through: %+v", res)
	}
	if res := r.Dispatch(ctx, "/unknown", "s1", nil, nil, &nopSink{}, nil); !res.Handled {
		t.Fatalf("unknown slash command must still be handled with a hint")
	}
}

func TestHandleListSkillCommands(t *testing.T) {
	h := &ChatHandler{SlashRouter: newSkillRouter()}
	w := httptest.NewRecorder()
	h.HandleListSkillCommands(w, httptest.NewRequest("GET", "/v1/skills/commands", nil))
	body := w.Body.String()
	if !strings.Contains(body, `"/deploy:ship"`) || !strings.Contains(body, `"[env]"`) || strings.Contains(body, "model-only") {
		t.Fatalf("body: %s", body)
	}
}

type recordingSubagents struct{ agent, prompt, session string }

func (r *recordingSubagents) RunSubagent(_ context.Context, sessionID, agent, prompt string) (string, error) {
	r.agent, r.prompt, r.session = agent, prompt, sessionID
	return "subagent result", nil
}

func TestSlashRouter_ForkSkillRunsInSubagent(t *testing.T) {
	r := NewSlashCommandRouter(nil, nil)
	r.SetSkills(listSkillRegistry{skills: []types.SkillMeta{
		{Name: "skill:research", DisplayName: "research", Spec: `{"context":"fork","agent":"Explore"}`},
		{Name: "skill:plain_fork", DisplayName: "plain-fork", Spec: `{"context":"fork"}`},
	}}, echoSkillExec{})
	// 未注入执行器：按普通技能内联。
	if res := r.Dispatch(context.Background(), "/research auth", "s1", nil, nil, &nopSink{}, nil); res.Handled || res.RewrittenInput == "" {
		t.Fatalf("without runner fork skills run inline: %+v", res)
	}
	subs := &recordingSubagents{}
	r.SetSubagents(subs)
	sink := &nopSink{}
	res := r.Dispatch(context.Background(), "/research auth", "s1", nil, nil, sink, nil)
	if !res.Handled || res.Response != "subagent result" || sink.text.String() != "subagent result" ||
		subs.agent != "Explore" || subs.session != "s1" || !strings.Contains(subs.prompt, "rendered skill:research") {
		t.Fatalf("fork: %+v %+v", res, subs)
	}
	r.Dispatch(context.Background(), "/plain-fork", "s1", nil, nil, &nopSink{}, nil)
	if subs.agent != "general-purpose" {
		t.Fatalf("agent defaults to general-purpose: %q", subs.agent)
	}
}
