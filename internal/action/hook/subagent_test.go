package hook

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestSubagentEvents(t *testing.T) {
	src := Source{Key: "user:s", Scope: ScopeUser, Trusted: true, Config: mustConfig(t, `{"hooks":{
	  "SubagentStart":[{"matcher":"review:.*","hooks":[{"type":"command","command":"echo use-checklist"}]}],
	  "SubagentStop":[{"hooks":[{"type":"command","command":"in=$(cat); case \"$in\" in *'\"stop_hook_active\":true'*) exit 0;; esac; echo '{\"decision\":\"block\",\"reason\":\"add tests\"}'"}]}]}}`)}
	r := newTestRunner(t, src)
	ctx := context.Background()

	if got := r.FireSubagentStart(ctx, "s1", "a1", "review:security"); got != "use-checklist" {
		t.Fatalf("SubagentStart plain stdout becomes context: %q", got)
	}
	if got := r.FireSubagentStart(ctx, "s1", "a1", "Explore"); got != "" {
		t.Fatalf("matcher is the agent type: %q", got)
	}
	if got := r.FireSubagentStop(ctx, "s1", "a1", "Explore", "done", false); got != "add tests" {
		t.Fatalf("decision:block asks the subagent to continue: %q", got)
	}
	if got := r.FireSubagentStop(ctx, "s1", "a1", "Explore", "done", true); got != "" {
		t.Fatalf("stop_hook_active lets the hook allow stopping: %q", got)
	}
	suppressed := context.WithValue(ctx, protocol.CtxHooksSuppressedKey{}, true)
	if got := r.FireSubagentStart(suppressed, "s1", "a1", "review:security"); got != "" {
		t.Fatal("suppressed context must not run hooks")
	}
}
