package job_kill

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

func TestJobKill(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "polaris_job_kill_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	jm := bash.NewJobManager(tmpDir)
	fn := MakeJobKillFn(jm)

	ctx := context.Background()

	// 启动一个长运行睡眠任务
	meta, err := jm.StartJob(ctx, "sleep 30", tmpDir, "sess_kill", nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	// 调用 job_kill
	killBytes, _ := json.Marshal(map[string]string{"job_id": meta.JobID})
	res, err := fn(ctx, killBytes)
	if err != nil {
		t.Fatalf("job_kill failed: %v", err)
	}

	var killMeta bash.JobMeta
	if err := json.Unmarshal(res, &killMeta); err != nil {
		t.Fatal(err)
	}

	if killMeta.JobID != meta.JobID {
		t.Fatalf("expected job_id %s, got %s", meta.JobID, killMeta.JobID)
	}
	if killMeta.Status != bash.JobStatusKilled {
		t.Fatalf("expected status %s, got %s", bash.JobStatusKilled, killMeta.Status)
	}

	// 再次查询确认状态确实为 killed
	queried, err := jm.GetJob(meta.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if queried.Status != bash.JobStatusKilled {
		t.Fatalf("expected queried status %s, got %s", bash.JobStatusKilled, queried.Status)
	}
}
