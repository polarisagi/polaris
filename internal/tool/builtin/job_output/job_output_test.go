package job_output

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

func TestJobOutput(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "polaris_job_output_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	jm := bash.NewJobManager(tmpDir)
	fn := MakeJobOutputFn(jm)

	ctx := context.Background()

	// 启动一个多行输出任务
	cmd := "for i in {1..10}; do echo \"line $i\"; done"
	meta, err := jm.StartJob(ctx, cmd, tmpDir, "sess_output", nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond)

	// 获取尾部 5 行
	reqBytes, _ := json.Marshal(map[string]any{
		"job_id":     meta.JobID,
		"tail_lines": 5,
	})
	res, err := fn(ctx, reqBytes)
	if err != nil {
		t.Fatalf("job_output failed: %v", err)
	}

	var outResp struct {
		JobID     string `json:"job_id"`
		Output    string `json:"output"`
		Status    string `json:"status"`
		ExitCode  int    `json:"exit_code"`
		TailLines int    `json:"tail_lines"`
	}
	if err := json.Unmarshal(res, &outResp); err != nil {
		t.Fatal(err)
	}

	if outResp.JobID != meta.JobID {
		t.Fatalf("expected job_id %s, got %s", meta.JobID, outResp.JobID)
	}
	if outResp.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", outResp.ExitCode)
	}
	lines := strings.Split(strings.TrimSpace(outResp.Output), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d: %q", len(lines), outResp.Output)
	}
	if lines[len(lines)-1] != "line 10" {
		t.Fatalf("expected last line to be 'line 10', got %q", lines[len(lines)-1])
	}
}
