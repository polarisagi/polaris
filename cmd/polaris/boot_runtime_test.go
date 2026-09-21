package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/runtimeinfo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func newTestLayout(t *testing.T) config.DataLayout {
	t.Helper()
	layout := config.NewDataLayout(t.TempDir(), config.DirsConfig{})
	if err := layout.MkdirAll(); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return layout
}

// 单实例锁：第二次获取必须失败。
// flock 绑定的是 open file description，同进程内两次 open 是两个 description，
// 故本用例无需起子进程即可覆盖"第二个实例"的语义。
func TestAcquireRuntimeIsExclusive(t *testing.T) {
	layout := newTestLayout(t)

	first, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("首次获取应成功: %v", err)
	}
	defer first.release()

	second, err := acquireRuntime(layout)
	if err == nil {
		second.release()
		t.Fatal("第二个实例取得了锁——单实例约束失效")
	}
	if !apperr.IsCode(err, apperr.CodeAlreadyExists) {
		t.Fatalf("期望 CodeAlreadyExists，实际 %v", err)
	}
}

// 释放后可再次获取（进程重启场景）。
func TestAcquireRuntimeAfterRelease(t *testing.T) {
	layout := newTestLayout(t)

	first, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}
	first.release()

	second, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("释放后应能再次获取: %v", err)
	}
	second.release()
}

// publish 写的是实际端口与本次启动的令牌，且 release 后不留残留。
func TestPublishAndRelease(t *testing.T) {
	layout := newTestLayout(t)

	h, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("acquireRuntime: %v", err)
	}
	if err = h.publish(45678); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got, err := runtimeinfo.Read(runtimeinfo.Paths{PID: layout.RunPID, Port: layout.RunPort, Token: layout.RunToken})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Port != 45678 {
		t.Fatalf("端口应为 45678，实际 %d", got.Port)
	}
	if got.Token != h.Token {
		t.Fatal("落盘令牌与进程持有的令牌不一致")
	}
	if got.PID != os.Getpid() {
		t.Fatalf("PID 应为 %d，实际 %d", os.Getpid(), got.PID)
	}

	h.release()
	for _, p := range []string{layout.RunPID, layout.RunPort, layout.RunToken} {
		if _, err = os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应在 release 后清理", filepath.Base(p))
		}
	}
	h.release() // 幂等
}

// 每次启动的令牌必须轮换，不得复用上一次的值。
func TestTokenRotatesPerStart(t *testing.T) {
	layout := newTestLayout(t)

	first, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("acquireRuntime: %v", err)
	}
	tokenA := first.Token
	first.release()

	second, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("acquireRuntime: %v", err)
	}
	defer second.release()

	if second.Token == tokenA {
		t.Fatal("两次启动的本地令牌相同——令牌未轮换")
	}
}

// probeInstanceLock 是"守护进程是否在运行"的权威判据（不看 run/ 文件）。
// 负向面：kill -9 后文件残留、锁已释放——必须判为未运行。
func TestProbeInstanceLock(t *testing.T) {
	layout := newTestLayout(t)

	if probeInstanceLock(layout.RunLock) {
		t.Fatal("锁文件不存在时应判为未运行")
	}

	h, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("acquireRuntime: %v", err)
	}
	if err = h.publish(40000); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !probeInstanceLock(layout.RunLock) {
		t.Fatal("持锁期间应判为运行中")
	}

	// 模拟 kill -9：只释放锁、不清理 run/ 文件。
	unlockInstance(h.lock)
	h.lock = nil
	if _, statErr := os.Stat(layout.RunPort); statErr != nil {
		t.Fatalf("前置：端口文件应仍残留: %v", statErr)
	}
	if probeInstanceLock(layout.RunLock) {
		t.Fatal("锁已释放但文件残留时应判为未运行——否则客户端会连向没人监听的端口")
	}

	// 探测本身不得留下占用：探测之后应能正常取锁。
	h2, err := acquireRuntime(layout)
	if err != nil {
		t.Fatalf("探测后应能再次取锁: %v", err)
	}
	h2.release()
}
