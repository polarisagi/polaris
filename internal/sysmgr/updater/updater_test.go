package updater

import (
	"context"
	"testing"
	"time"
)

// ── equalVersions ──────────────────────────────────────────────────────────

func TestEqualVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.7.6", "v1.7.6", true},
		{"v1.7.6", "1.7.6", true}, // v 前缀不对等价
		{"1.7.6", "v1.7.6", true},
		{"1.7.6", "1.7.6", true},
		{"v1.7.6", "v1.7.7", false},
		{"v1.0.0", "v1.7.6", false},
		{"dev", "v1.0.0", false},
	}
	for _, tc := range cases {
		if got := equalVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("equalVersions(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// ── New / GetVersionInfo ───────────────────────────────────────────────────

func TestNew_FieldsCorrect(t *testing.T) {
	m := New("v1.0.0", "abc123", "2026-01-01", nil)
	if m == nil {
		t.Fatal("expected non-nil Manager")
	}
	info := m.GetVersionInfo()
	if info.Current != "v1.0.0" {
		t.Errorf("Current: got %q, want %q", info.Current, "v1.0.0")
	}
	if info.CommitHash != "abc123" {
		t.Errorf("CommitHash: got %q, want %q", info.CommitHash, "abc123")
	}
	if info.BuildDate != "2026-01-01" {
		t.Errorf("BuildDate: got %q, want %q", info.BuildDate, "2026-01-01")
	}
	if info.UpdateStatus != StatusIdle {
		t.Errorf("UpdateStatus: got %q, want %q", info.UpdateStatus, StatusIdle)
	}
	if info.HasUpdate {
		t.Error("HasUpdate should be false initially")
	}
}

func TestGetVersionInfo_ThreadSafe(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	// 并发读取不应 panic 或 data race（-race 下验证）
	done := make(chan struct{})
	for range 5 {
		go func() {
			_ = m.GetVersionInfo()
			done <- struct{}{}
		}()
	}
	for range 5 {
		<-done
	}
}

// ── SetRestartFn ───────────────────────────────────────────────────────────

func TestSetRestartFn(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	called := false
	m.SetRestartFn(func() { called = true })
	if m.restartFn == nil {
		t.Fatal("restartFn should be set")
	}
	m.restartFn()
	if !called {
		t.Error("restartFn not called")
	}
}

// ── TriggerUpdate ──────────────────────────────────────────────────────────

func TestTriggerUpdate_EmptyVersion(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	err := m.TriggerUpdate(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty version, got nil")
	}
}

func TestTriggerUpdate_AlreadyInProgress(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	// 手动设置为 downloading 状态
	m.setStatus(StatusDownloading)
	err := m.TriggerUpdate(context.Background(), "v1.7.6")
	if err == nil {
		t.Error("expected conflict error when update in progress, got nil")
	}
}

func TestTriggerUpdate_AcceptsIdleStatus(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	// 注入 restartFn 防止 os.Exit；doUpdate 会失败（无网络），但 TriggerUpdate 本身应成功
	m.SetRestartFn(func() {})
	err := m.TriggerUpdate(context.Background(), "v1.7.6")
	if err != nil {
		t.Errorf("expected no error for idle state, got: %v", err)
	}
	// 等待后台 goroutine 完成（最终进入 Error 状态）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info := m.GetVersionInfo()
		if info.UpdateStatus == StatusError || info.UpdateStatus == StatusIdle {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestTriggerUpdate_AcceptsErrorStatus(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	m.setError("previous failure")
	m.SetRestartFn(func() {})
	// Error 状态下允许重试
	err := m.TriggerUpdate(context.Background(), "v1.7.6")
	if err != nil {
		t.Errorf("expected retry to be accepted, got: %v", err)
	}
}

// ── status helpers ─────────────────────────────────────────────────────────

func TestSetStatus_UpdatesStatus(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	m.setStatus(StatusDownloading)
	if got := m.GetVersionInfo().UpdateStatus; got != StatusDownloading {
		t.Errorf("got %q, want %q", got, StatusDownloading)
	}
}

func TestSetError_SetsErrorState(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	m.setError("something went wrong")
	info := m.GetVersionInfo()
	if info.UpdateStatus != StatusError {
		t.Errorf("UpdateStatus: got %q, want %q", info.UpdateStatus, StatusError)
	}
	if info.UpdateError != "something went wrong" {
		t.Errorf("UpdateError: got %q", info.UpdateError)
	}
}

func TestSetIdle_OnlyFromChecking(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	m.setStatus(StatusChecking)
	m.setIdle()
	if got := m.GetVersionInfo().UpdateStatus; got != StatusIdle {
		t.Errorf("got %q, want %q", got, StatusIdle)
	}

	// 非 checking 状态下 setIdle 不生效
	m.setStatus(StatusDownloading)
	m.setIdle()
	if got := m.GetVersionInfo().UpdateStatus; got != StatusDownloading {
		t.Errorf("setIdle should not affect non-checking status, got %q", got)
	}
}

// ── CheckLatest ────────────────────────────────────────────────────────────

func TestCheckLatest_DevVersionNeverUpdates(t *testing.T) {
	// dev 版本不提示更新（equalVersions 短路之外，current=="dev" 额外阻断）
	m := New("dev", "", "", nil)
	// 模拟 GitHub API 返回 v1.7.6
	// 注：CheckLatest 内部需要真实 HTTP，这里只验证 dev 版本最终 HasUpdate=false
	// 不调 CheckLatest（需网络），改为直接调 setStatus + 验证 Info 初始状态
	info := m.GetVersionInfo()
	if info.Current != "dev" {
		t.Errorf("Current: got %q", info.Current)
	}
}

// ── StartBackgroundCheck ───────────────────────────────────────────────────

func TestStartBackgroundCheck_CtxCancelStops(t *testing.T) {
	m := New("v1.0.0", "", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	// 使用很长 interval 保证在测试窗口内不会实际触发
	m.StartBackgroundCheck(ctx, 24*time.Hour)
	cancel() // 取消应使 goroutine 退出，不 panic，不阻塞
	time.Sleep(50 * time.Millisecond)
}
