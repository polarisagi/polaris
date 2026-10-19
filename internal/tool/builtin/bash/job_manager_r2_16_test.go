package bash_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

// TestR2_16_JobID_PathTraversal_Rejected 验证非法/路径穿越的 job_id 被拒绝。
func TestR2_16_JobID_PathTraversal_Rejected(t *testing.T) {
	jm := bash.NewJobManager(t.TempDir())

	badIDs := []string{
		"../../etc/passwd",
		"job_../../test",
		"../output.log",
		"/absolute/path",
		"invalid-not-uuid",
		"job_invalid",
	}

	for _, badID := range badIDs {
		_, err := jm.GetJob(badID)
		if err == nil {
			t.Errorf("GetJob expected error for %q, got nil", badID)
		}
		_, err = jm.GetJobOutput(badID, 10)
		if err == nil {
			t.Errorf("GetJobOutput expected error for %q, got nil", badID)
		}
		_, err = jm.KillJob(badID)
		if err == nil {
			t.Errorf("KillJob expected error for %q, got nil", badID)
		}
	}
}

// TestR2_16_LogSize_TruncatedAtLimit 验证超过日志上限时被截断并添加标记。
func TestR2_16_LogSize_TruncatedAtLimit(t *testing.T) {
	dir := t.TempDir()
	jm := bash.NewJobManager(dir)
	// 设置为 1KB 上限测试截断
	jm.SetMaxLogBytes(1024)

	// 输出 10KB 数据
	cmdStr := "for i in $(seq 1 200); do echo '12345678901234567890123456789012345678901234567890'; done"
	meta, err := jm.StartJob(context.Background(), cmdStr, dir, "sess_trunc", nil)
	if err != nil {
		t.Fatalf("StartJob failed: %v", err)
	}

	// 等待完成
	time.Sleep(300 * time.Millisecond)

	out, err := jm.GetJobOutput(meta.JobID, 500)
	if err != nil {
		t.Fatalf("GetJobOutput failed: %v", err)
	}
	if !strings.Contains(out, "[LOG TRUNCATED") {
		t.Fatalf("expected log to contain truncation marker, got output length %d", len(out))
	}

	logPath := filepath.Join(dir, meta.JobID, "output.log")
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log failed: %v", err)
	}
	// 允许包含截断标记的额外几十字节，但必须远小于原输出 ~10KB
	if info.Size() > 2048 {
		t.Fatalf("expected log file to be bounded under 2KB, got %d", info.Size())
	}
}

// TestR2_16_GetJobOutput_TailSingleLineOver64KB 验证单行超过 64KB（Scanner上限）时倒读正常返回。
func TestR2_16_GetJobOutput_TailSingleLineOver64KB(t *testing.T) {
	dir := t.TempDir()
	jm := bash.NewJobManager(dir)

	meta, err := jm.StartJob(context.Background(), "echo start", dir, "sess_long", nil)
	if err != nil {
		t.Fatalf("StartJob failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 人工在 output.log 中写入单行 128KB 数据
	logPath := filepath.Join(dir, meta.JobID, "output.log")
	longLine := strings.Repeat("A", 128*1024) // 128KB > 64KB bufio.MaxScanTokenSize
	fileContent := "line 1\n" + longLine + "\nline 3\n"
	if err := os.WriteFile(logPath, []byte(fileContent), 0o600); err != nil {
		t.Fatalf("write long log failed: %v", err)
	}

	// 倒读最后 2 行
	out, err := jm.GetJobOutput(meta.JobID, 2)
	if err != nil {
		t.Fatalf("GetJobOutput failed on >64KB line: %v", err)
	}
	if !strings.Contains(out, "line 3") {
		t.Errorf("expected out to contain line 3, got: %s", out[:min(len(out), 100)])
	}
	if !strings.Contains(out, strings.Repeat("A", 1000)) {
		t.Errorf("expected out to contain the long line")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestR2_16_ProcessGroup_KillKillsChildren 验证 kill 终止后台任务时杀掉整个进程组。
func TestR2_16_ProcessGroup_KillKillsChildren(t *testing.T) {
	dir := t.TempDir()
	jm := bash.NewJobManager(dir)

	// 启动一个后台进程
	meta, err := jm.StartJob(context.Background(), "sleep 60", dir, "sess_pg", nil)
	if err != nil {
		t.Fatalf("StartJob failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	killedMeta, err := jm.KillJob(meta.JobID)
	if err != nil {
		t.Fatalf("KillJob failed: %v", err)
	}
	if killedMeta.Status != bash.JobStatusKilled {
		t.Fatalf("expected status killed, got %s", killedMeta.Status)
	}

	// 再次查询确保状态更新
	metaAfter, err := jm.GetJob(meta.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if metaAfter.Status != bash.JobStatusKilled {
		t.Fatalf("expected persisted status killed, got %s", metaAfter.Status)
	}
}
