package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func authzTool(t *testing.T, f *permFixture, name string) types.Tool {
	t.Helper()
	tl, ok := f.ag.decisionTool(name)
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	return tl
}

// full_access：桥内写入放行且凭据为 Explicit。
func TestAuthorizeCall_FullAccessWriteExplicit(t *testing.T) {
	f := newPermFixture(t, types.ModeFullAccess, "p1", &memRules{}, nil, denyAll)
	args := []byte(`{"path":"` + f.ag.workspaceRoot + `/a.txt","content":"x"}`)
	g, reason, ok := f.ag.AuthorizeCall(context.Background(), authzTool(t, f, "write_file"), args, types.TaintLow)
	if !ok || g != protocol.GrantExplicit {
		t.Fatalf("want explicit allow, got grant=%v ok=%v reason=%q", g, ok, reason)
	}
}

// auto_review：工作区外写入需审批，桥内直接拒绝并带原因。
func TestAuthorizeCall_AutoReviewOutsideWriteRejected(t *testing.T) {
	f := newPermFixture(t, types.ModeAutoReview, "p1", &memRules{}, nil, denyAll)
	outside := t.TempDir()
	args := []byte(`{"path":"` + outside + `/a.txt","content":"x"}`)
	g, reason, ok := f.ag.AuthorizeCall(context.Background(), authzTool(t, f, "write_file"), args, types.TaintLow)
	if ok || g != protocol.GrantNone || strings.TrimSpace(reason) == "" {
		t.Fatalf("want rejection with reason, got grant=%v ok=%v reason=%q", g, ok, reason)
	}
}

// 外层 ctx 为 Explicit 时，auto_review 内层 safe 命令拿到 Auto，不继承外层凭据。
func TestAuthorizeCall_InnerGrantNotInherited(t *testing.T) {
	f := newPermFixture(t, types.ModeAutoReview, "p1", &memRules{}, nil, denyAll)
	outer := protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit)
	g, reason, ok := f.ag.AuthorizeCall(outer, authzTool(t, f, "bash"), []byte(`{"command":"ls"}`), types.TaintLow)
	if !ok || g != protocol.GrantAuto {
		t.Fatalf("want GrantAuto for safe command, got grant=%v ok=%v reason=%q", g, ok, reason)
	}
}
