package sandbox

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	isandbox "github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type recordingPhysicalWrapper struct {
	ctx protocol.SandboxContext
}

func (w *recordingPhysicalWrapper) WrapArgv(_ context.Context, ctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	w.ctx = ctx
	return &protocol.WrapArgvResult{
		Executable:    ctx.ExecPath,
		Argv:          ctx.ExecArgs,
		Env:           []string{"PATH=/usr/bin:/bin"},
		SandboxMethod: "seatbelt",
	}, nil
}

func TestBoundedCmdRunner_RequiresNetworkBlockAndPhysicalSandbox(t *testing.T) {
	wrapper := &recordingPhysicalWrapper{}
	runner := &BoundedCmdRunner{wrapper: wrapper}
	base := isandbox.CmdRunnerCfg{Command: "echo valid", WorkDir: t.TempDir(), NetworkBlock: true}
	_, code, method, err := runner.RunCmd(context.Background(), base)
	if err != nil || code != 0 || method != "seatbelt" {
		t.Fatalf("run: code=%d method=%q err=%v", code, method, err)
	}
	if wrapper.ctx.NetworkPolicy != protocol.NetPolicyDeny {
		t.Fatalf("network policy = %q", wrapper.ctx.NetworkPolicy)
	}
	// Verify caller deny paths are forwarded as well as host defaults.
	base.DenyPaths = []string{"/custom/deny"}
	_, _, _, err = runner.RunCmd(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range wrapper.ctx.DenyPaths {
		if path == "/custom/deny" {
			return
		}
	}
	t.Fatal("caller deny path was not forwarded")
}

func TestBoundedCmdRunner_BoundsOutputAndRejectsTimeout(t *testing.T) {
	runner := &BoundedCmdRunner{wrapper: &recordingPhysicalWrapper{}}
	cfg := isandbox.CmdRunnerCfg{
		Command: "head -c 100000 /dev/zero; head -c 100000 /dev/zero >&2",
		WorkDir: t.TempDir(), NetworkBlock: true,
	}
	output, code, _, err := runner.RunCmd(context.Background(), cfg)
	if err != nil || code != 0 || len(output) != 2*boundedCmdOutputLimit {
		t.Fatalf("output bound: len=%d code=%d err=%v", len(output), code, err)
	}
	cfg.Command = "sleep 5"
	cfg.TimeoutMs = 50
	_, code, _, err = runner.RunCmd(context.Background(), cfg)
	if code != -1 || !apperr.IsCode(err, apperr.CodeTimeout) {
		t.Fatalf("timeout: code=%d err=%v", code, err)
	}
}

func TestBoundedCmdRunner_RefusesNetworkEnabledConfig(t *testing.T) {
	runner := &BoundedCmdRunner{wrapper: &recordingPhysicalWrapper{}}
	_, _, _, err := runner.RunCmd(context.Background(), isandbox.CmdRunnerCfg{Command: "true"})
	if !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected network fail-closed error, got %v", err)
	}
}
