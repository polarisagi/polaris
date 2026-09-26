package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// passthroughWrapper 测试用：不加沙箱，原样返回 argv（验证进程 IO 语义，不验证隔离）。
type passthroughWrapper struct{ fail bool }

func (w passthroughWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	if w.fail {
		return nil, errors.New("no sandbox available")
	}
	return &protocol.WrapArgvResult{Executable: sctx.ExecPath, Argv: sctx.ExecArgs, Env: append([]string{"PATH=/usr/bin:/bin"}, sctx.EnvExtra...)}, nil
}

func TestRunStdio_StdinStdoutStderrExitCode(t *testing.T) {
	res, err := RunStdio(context.Background(), passthroughWrapper{}, StdioRequest{
		Command: `read -r line; echo "out:$line:$HOOK_VAR"; echo "why" >&2; exit 2`,
		Stdin:   []byte("{\"x\":1}\n"),
		Env:     []string{"HOOK_VAR=v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(res.Stdout)) != `out:{"x":1}:v` || strings.TrimSpace(string(res.Stderr)) != "why" || res.ExitCode != 2 {
		t.Fatalf("got stdout=%q stderr=%q exit=%d", res.Stdout, res.Stderr, res.ExitCode)
	}
}

func TestRunStdio_ExecFormDoesNotReparseArgs(t *testing.T) {
	res, err := RunStdio(context.Background(), passthroughWrapper{}, StdioRequest{
		ExecPath: "/bin/echo", Args: []string{"$(whoami)", "a;b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "$(whoami) a;b" {
		t.Fatalf("exec-form args must be passed literally, got %q", res.Stdout)
	}
}

func TestRunStdio_TimeoutAndOutputLimit(t *testing.T) {
	res, err := RunStdio(context.Background(), passthroughWrapper{}, StdioRequest{Command: "sleep 5", Timeout: 100 * time.Millisecond})
	if err != nil || !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	res, err = RunStdio(context.Background(), passthroughWrapper{}, StdioRequest{Command: "head -c 5000 /dev/zero", MaxOutput: 100})
	if err != nil || len(res.Stdout) != 100 {
		t.Fatalf("limit: len=%d err=%v", len(res.Stdout), err)
	}
}

func TestRunStdio_FailClosed(t *testing.T) {
	if _, err := RunStdio(context.Background(), nil, StdioRequest{Command: "true"}); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("nil wrapper must fail closed, got %v", err)
	}
	if _, err := RunStdio(context.Background(), passthroughWrapper{fail: true}, StdioRequest{Command: "true"}); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("wrap failure must fail closed, got %v", err)
	}
}
