package bash_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

type bareWrapper struct{}

func (bareWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return &protocol.WrapArgvResult{
		Executable: sctx.ExecPath,
		Argv:       append([]string{sctx.ExecPath}, sctx.ExecArgs...),
		Env:        sctx.EnvExtra,
		EnvInArgv:  false,
	}, nil
}

func TestMakeBashFn_BackgroundJob(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", pol)

	ctx := protocol.WithPreApproved(context.Background())
	input := `{"command":"echo 'job_bg_done'","background":true,"session_id":"s_bg_1"}`
	res, err := fn(ctx, []byte(input))
	if err != nil {
		t.Fatalf("background bash execution failed: %v", err)
	}

	var resp struct {
		JobID      string `json:"job_id"`
		Status     string `json:"status"`
		Background bool   `json:"background"`
		SessionID  string `json:"session_id"`
	}
	if err := json.Unmarshal(res, &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if resp.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}
	if !resp.Background {
		t.Fatal("expected background=true")
	}

	// 等待任务执行完成
	time.Sleep(150 * time.Millisecond)

	jm := bash.GetDefaultJobManager()
	meta, err := jm.GetJob(resp.JobID)
	if err != nil {
		t.Fatalf("get job failed: %v", err)
	}
	if meta.Status != bash.JobStatusCompleted {
		t.Fatalf("expected completed status, got %s", meta.Status)
	}
	if meta.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", meta.ExitCode)
	}

	out, err := jm.GetJobOutput(resp.JobID, 10)
	if err != nil {
		t.Fatalf("get job output failed: %v", err)
	}
	if !strings.Contains(out, "job_bg_done") {
		t.Fatalf("expected output to contain 'job_bg_done', got %q", out)
	}
}

func TestMakeBashFn_PersistentSession_StatePersists(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)

	pSbx := sandbox.NewPersistentSandbox(bareWrapper{}, sandbox.PersistentSandboxConfig{
		IdleTTL:      1 * time.Minute,
		MaxSessions:  4,
		ExecTimeout:  10 * time.Second,
		ReapInterval: 10 * time.Second,
	})
	defer pSbx.Shutdown()

	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", pol, pSbx)
	ctx := protocol.WithPreApproved(context.Background())

	// 1. 设置环境变量与函数
	in1 := `{"command":"export TEST_AUTONOMY_VAR='polaris_42'\nmy_fn() { echo \"fn_$1\"; }","session_id":"sess_term_1"}`
	res1, err := fn(ctx, []byte(in1))
	if err != nil {
		t.Fatalf("step 1 failed: %v", err)
	}
	var r1 struct {
		ExitCode   int  `json:"exit_code"`
		Persistent bool `json:"persistent"`
	}
	if err := json.Unmarshal(res1, &r1); err != nil {
		t.Fatal(err)
	}
	if !r1.Persistent || r1.ExitCode != 0 {
		t.Fatalf("step 1 unexpected result: %+v", r1)
	}

	// 2. 在同一会话下读取变量并调用函数
	in2 := `{"command":"echo \"VAR=$TEST_AUTONOMY_VAR\"\nmy_fn hello","session_id":"sess_term_1"}`
	res2, err := fn(ctx, []byte(in2))
	if err != nil {
		t.Fatalf("step 2 failed: %v", err)
	}
	var r2 struct {
		Output     string `json:"output"`
		ExitCode   int    `json:"exit_code"`
		Persistent bool   `json:"persistent"`
	}
	if err := json.Unmarshal(res2, &r2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r2.Output, "VAR=polaris_42") {
		t.Fatalf("expected variable to persist, got output: %q", r2.Output)
	}
	if !strings.Contains(r2.Output, "fn_hello") {
		t.Fatalf("expected function to persist, got output: %q", r2.Output)
	}
}

func TestJobManager_CleanupSessionJobs(t *testing.T) {
	tmpDir := t.TempDir()
	jm := bash.NewJobManager(tmpDir)
	ctx := context.Background()

	job1, err := jm.StartJob(ctx, "sleep 30", tmpDir, "sess_clean_A", nil)
	if err != nil {
		t.Fatal(err)
	}
	job2, err := jm.StartJob(ctx, "sleep 30", tmpDir, "sess_clean_B", nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	// 清理 sess_clean_A 的任务
	jm.CleanupSessionJobs("sess_clean_A")

	time.Sleep(50 * time.Millisecond)

	meta1, err := jm.GetJob(job1.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if meta1.Status != bash.JobStatusKilled {
		t.Fatalf("expected job1 killed, got %s", meta1.Status)
	}

	meta2, err := jm.GetJob(job2.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.Status != bash.JobStatusRunning {
		t.Fatalf("expected job2 still running, got %s", meta2.Status)
	}

	// 彻底终止 job2
	_, _ = jm.KillJob(job2.JobID)
}
