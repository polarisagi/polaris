package job_status

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

func TestJobStatus(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "polaris_job_status_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	jm := bash.NewJobManager(tmpDir)
	fn := MakeJobStatusFn(jm)

	ctx := context.Background()

	// 1. 无任务时列表为空
	res, err := fn(ctx, []byte("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var listResp struct {
		Jobs []bash.JobMeta `json:"jobs"`
	}
	if err := json.Unmarshal(res, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(listResp.Jobs))
	}

	// 2. 启动一个任务
	meta, err := jm.StartJob(ctx, "echo 'hello'", tmpDir, "sess_1", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 等待任务完成
	time.Sleep(100 * time.Millisecond)

	// 3. 按 job_id 查询
	queryBytes, _ := json.Marshal(map[string]string{"job_id": meta.JobID})
	res, err = fn(ctx, queryBytes)
	if err != nil {
		t.Fatalf("query job error: %v", err)
	}
	var singleMeta bash.JobMeta
	if err := json.Unmarshal(res, &singleMeta); err != nil {
		t.Fatal(err)
	}
	if singleMeta.JobID != meta.JobID {
		t.Fatalf("expected job_id %s, got %s", meta.JobID, singleMeta.JobID)
	}
	if singleMeta.Status != bash.JobStatusCompleted {
		t.Fatalf("expected status %s, got %s", bash.JobStatusCompleted, singleMeta.Status)
	}

	// 4. 按 session_id 列表查询
	sessBytes, _ := json.Marshal(map[string]string{"session_id": "sess_1"})
	res, err = fn(ctx, sessBytes)
	if err != nil {
		t.Fatalf("list jobs error: %v", err)
	}
	if err := json.Unmarshal(res, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(listResp.Jobs))
	}
}
