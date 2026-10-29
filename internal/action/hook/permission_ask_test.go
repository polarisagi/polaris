package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestNewInput_PermissionModeMapping(t *testing.T) {
	cases := map[types.PermissionMode]string{
		types.ModeDefault:    "default",
		types.ModeAutoReview: "acceptEdits",
		types.ModeFullAccess: "bypassPermissions",
		"":                   "default",
	}
	for mode, want := range cases {
		ctx := protocol.WithPermissionMode(context.Background(), mode)
		if got := NewInput(ctx, EventPreToolUse).PermissionMode; got != want {
			t.Errorf("mode %q: got %q want %q", mode, got, want)
		}
	}
}

type fakeApprover struct {
	err    error
	action string
	req    map[string]any
}

func (f *fakeApprover) RequestApproval(_ context.Context, action string, req map[string]any) error {
	f.action, f.req = action, req
	return f.err
}

func newAskRunner(t *testing.T) *Runner {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(root, "ask.sh")
	body := `#!/bin/sh
cat >/dev/null
echo '{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"need ok"}}'`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	src := Source{Key: "user:x", Scope: ScopeUser, Trusted: true, PluginRoot: root, Config: mustConfig(t,
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/ask.sh"}]}]}}`)}
	return newTestRunner(t, src)
}

func TestPreToolUseAsk(t *testing.T) {
	ctx := context.Background()
	in := map[string]any{"command": "x"}

	r := newAskRunner(t)
	if res := r.FirePreToolUse(ctx, "bash", in); !res.Blocked {
		t.Fatalf("无审批通道时 ask 必须阻断: %+v", res)
	}

	ap := &fakeApprover{}
	r.SetApprover(ap)
	if res := r.FirePreToolUse(ctx, "bash", in); res.Blocked {
		t.Fatalf("批准后不应阻断: %+v", res)
	}
	if ap.action != "hook_ask" || ap.req["tool_name"] != "bash" || ap.req["reason"] != "need ok" {
		t.Fatalf("审批请求内容不符: %q %v", ap.action, ap.req)
	}

	ap.err = errors.New("rejected")
	if res := r.FirePreToolUse(ctx, "bash", in); !res.Blocked {
		t.Fatalf("拒绝后必须阻断: %+v", res)
	}
}
