package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

type invalidArgvWrapper struct{ result *protocol.WrapArgvResult }

func (w invalidArgvWrapper) WrapArgv(context.Context, protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return w.result, nil
}

type physicalPassthroughWrapper struct{}

func (physicalPassthroughWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return &protocol.WrapArgvResult{
		Executable:    sctx.ExecPath,
		Argv:          sctx.ExecArgs,
		Env:           []string{"PATH=/usr/bin:/bin"},
		SandboxMethod: "seatbelt",
	}, nil
}

func TestRunStdio_RequiresCompletePhysicalWrapperBeforeSpawn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	for name, wrapper := range map[string]ArgvWrapper{
		"nil result":       invalidArgvWrapper{},
		"empty executable": invalidArgvWrapper{result: &protocol.WrapArgvResult{}},
		"bare method":      passthroughWrapper{},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := RunStdio(context.Background(), wrapper, StdioRequest{Command: "touch " + marker, RequirePhysical: true})
			if !apperr.IsCode(err, apperr.CodeForbidden) {
				t.Fatalf("expected fail-closed physical sandbox error, got %v", err)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("command ran before physical method validation: stat err=%v", statErr)
			}
		})
	}
}

func TestRunStdio_CancelReturnsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunStdio(ctx, passthroughWrapper{}, StdioRequest{Command: "sleep 5"})
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Fatalf("expected cancelled code, got %v", err)
	}
}

func TestRunStdio_KillsDescendantsOnTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	res, err := RunStdio(context.Background(), passthroughWrapper{}, StdioRequest{
		Command: "(sleep 0.5; touch " + marker + ") & wait",
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || !res.TimedOut {
		t.Fatalf("expected timeout: result=%+v err=%v", res, err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("descendant survived process-group cancellation: stat err=%v", statErr)
	}
}

func TestRunStdio_PhysicalRunCleansBackgroundChildrenAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "background-survived")
	started := filepath.Join(dir, "background-started")
	res, err := RunStdio(context.Background(), physicalPassthroughWrapper{}, StdioRequest{
		Command:         "(touch " + started + "; sleep 0.5; touch " + marker + ") >/dev/null 2>&1 & while [ ! -f " + started + " ]; do sleep 0.01; done; exit 0",
		RequirePhysical: true,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected successful command: result=%+v err=%v", res, err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("background child survived physical execution cleanup: stat err=%v", statErr)
	}
}

func TestRunStdio_BoundsBothOutputStreams(t *testing.T) {
	res, err := RunStdio(context.Background(), passthroughWrapper{}, StdioRequest{
		Command:   "head -c 100000 /dev/zero; head -c 100000 /dev/zero >&2",
		MaxOutput: 128,
	})
	if err != nil || len(res.Stdout) != 128 || len(res.Stderr) != 128 {
		t.Fatalf("expected per-stream bounds: stdout=%d stderr=%d err=%v", len(res.Stdout), len(res.Stderr), err)
	}
}
