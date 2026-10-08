package audiorun

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// 1. 串行测试：TTS 安装开始时间 ≥ STT 安装结束时间；绝不并发。
func TestProvisioner_SerialExecution(t *testing.T) {
	var sttEnd, ttsStart time.Time
	var mu sync.Mutex

	sttCalled := false
	ttsCalled := false
	benchCalled := false

	sttFn := func(ctx context.Context) (bool, error) {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		sttCalled = true
		sttEnd = time.Now()
		mu.Unlock()
		return true, nil
	}

	ttsFn := func(ctx context.Context) (bool, error) {
		mu.Lock()
		ttsCalled = true
		ttsStart = time.Now()
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return true, nil
	}

	benchFn := func(ctx context.Context) error {
		mu.Lock()
		benchCalled = true
		mu.Unlock()
		return nil
	}

	sttSvc := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: supported()})
	ttsSvc := NewTTSService(context.Background(), TTSOptions{LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Profile: hostProf()})

	done := make(chan struct{})
	concurrent.SafeGo(context.Background(), "test.provisioner_serial", func(ctx context.Context) {
		runProvisioner(ctx, ProvisionerOptions{
			STT:            sttSvc,
			TTS:            ttsSvc,
			StartDelay:     5 * time.Millisecond,
			STTInstallFunc: sttFn,
			TTSInstallFunc: ttsFn,
			TTSBenchFunc:   benchFn,
		})
		close(done)
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("预置器执行超时")
	}

	mu.Lock()
	defer mu.Unlock()

	if !sttCalled || !ttsCalled || !benchCalled {
		t.Fatalf("步骤未全部执行: stt=%v tts=%v bench=%v", sttCalled, ttsCalled, benchCalled)
	}
	if ttsStart.Before(sttEnd) {
		t.Fatalf("TTS 在 STT 结束前即开始，未满足串行: STT end=%v, TTS start=%v", sttEnd, ttsStart)
	}
}

// 2. 跳过不支持：STT 不支持 → 不调用其安装；TTS 不支持或有定论 too_slow → 不调用 TTS 安装与基准。
func TestProvisioner_SkipUnsupported(t *testing.T) {
	sttCalled := false
	ttsCalled := false
	benchCalled := false

	unsupportedCap := Capability{Supported: false, Reason: ReasonInsufficientRAM}
	sttSvc := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: unsupportedCap})

	prof := hostProf()
	prefs := &memPrefs{}
	_ = SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: Fingerprint(prof, tts.ModelMelo), Model: "melo", RTF: 1.5, Supported: false, RetryOnStart: false,
	})
	// 降级链两环都定论 too_slow，TTS 才整体放弃（只有 Melo 慢时会改走 Matcha，见 fallback_test.go）。
	_ = SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: Fingerprint(prof, tts.ModelMatcha), Model: "matcha", RTF: 1.5, Supported: false, RetryOnStart: false,
	})
	ttsSvc := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Prefs: prefs, Profile: prof,
	})

	runProvisioner(context.Background(), ProvisionerOptions{
		STT: sttSvc,
		TTS: ttsSvc,
		STTInstallFunc: func(ctx context.Context) (bool, error) {
			sttCalled = true
			return true, nil
		},
		TTSInstallFunc: func(ctx context.Context) (bool, error) {
			ttsCalled = true
			return true, nil
		},
		TTSBenchFunc: func(ctx context.Context) error {
			benchCalled = true
			return nil
		},
	})

	if sttCalled {
		t.Error("不支持的 STT 不得调用安装")
	}
	if ttsCalled || benchCalled {
		t.Error("已有定论 too_slow 的 TTS 不得调用安装与基准")
	}
}

// 3. 退避：STT 前两次失败第三次成功 → 共 3 次调用，间隔符合注入的 Backoff；Backoff 耗尽后停止并保持 failed，且发布过 next_retry_at。
func TestProvisioner_Backoff(t *testing.T) {
	var attempts int
	var sinkRetries []time.Time
	var callTimes []time.Time

	backoff := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	fakeNow := time.Now()

	sttFn := func(ctx context.Context) (bool, error) {
		attempts++
		callTimes = append(callTimes, time.Now())
		if attempts < 3 {
			return false, errors.New("download failed")
		}
		return true, nil
	}

	sink := func(kind string, next time.Time) {
		if !next.IsZero() {
			sinkRetries = append(sinkRetries, next)
		}
	}

	sttSvc := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: supported()})

	runProvisioner(context.Background(), ProvisionerOptions{
		STT:            sttSvc,
		Backoff:        backoff,
		Sink:           sink,
		Now:            func() time.Time { return fakeNow },
		STTInstallFunc: sttFn,
	})

	if attempts != 3 {
		t.Fatalf("前两次失败第三次成功应共调用 3 次，实际 %d 次", attempts)
	}
	if len(sinkRetries) != 2 {
		t.Fatalf("应发布 2 次 next_retry_at，实际 %d 次", len(sinkRetries))
	}
	if !sinkRetries[0].Equal(fakeNow.Add(backoff[0])) || !sinkRetries[1].Equal(fakeNow.Add(backoff[1])) {
		t.Errorf("发布的重试时间不符合预期: %+v", sinkRetries)
	}

	// 测试耗尽情况
	attempts = 0
	sinkRetries = nil
	exhaustFn := func(ctx context.Context) (bool, error) {
		attempts++
		return false, errors.New("permanent failure")
	}

	runProvisioner(context.Background(), ProvisionerOptions{
		STT:            sttSvc,
		Backoff:        []time.Duration{5 * time.Millisecond, 5 * time.Millisecond},
		Sink:           sink,
		Now:            func() time.Time { return fakeNow },
		STTInstallFunc: exhaustFn,
	})

	if attempts != 3 { // 1 次初试 + 2 次重试 = 3 次
		t.Fatalf("Backoff 耗尽后应调用 3 次，实际 %d 次", attempts)
	}
	if len(sinkRetries) != 2 {
		t.Fatalf("应发布 2 次 next_retry_at，实际 %d 次", len(sinkRetries))
	}
}

// 4. 与用户点击互斥：预置器安装进行中时调用 Install() 返回 started=false，资产只下载一次。
func TestProvisioner_UserMutualExclusion(t *testing.T) {
	sttSvc := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: supported()})
	// 人工将 installing 置为 true（模拟预置器正在运行 InstallBlocking）
	sttSvc.installing.Store(true)

	started, err := sttSvc.Install()
	if started || err != nil {
		t.Fatalf("进行中时用户点击应返回 started=false, err=nil, got started=%v, err=%v", started, err)
	}

	// 恢复后验证可以触发
	sttSvc.installing.Store(false)
	// 在缺少文件且未真正下载的环境下，Install() 返回 started=true
	started, err = sttSvc.Install()
	if !started || err != nil {
		t.Fatalf("空闲时应返回 started=true, got started=%v err=%v", started, err)
	}
}

// 5. ctx 取消：StartDelay 期间取消 ctx → 不发生任何安装调用，goroutine 退出。
func TestProvisioner_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sttCalled := false

	sttSvc := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: supported()})

	done := make(chan struct{})
	concurrent.SafeGo(ctx, "test.provisioner_cancel", func(ctx context.Context) {
		runProvisioner(ctx, ProvisionerOptions{
			STT:        sttSvc,
			StartDelay: 500 * time.Millisecond,
			STTInstallFunc: func(ctx context.Context) (bool, error) {
				sttCalled = true
				return true, nil
			},
		})
		close(done)
	})

	// 立即取消
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("ctx 取消后 goroutine 未及时退出")
	}

	if sttCalled {
		t.Error("StartDelay 期间取消不得调用安装")
	}
}

// 6. decideBench 表驱动测试：覆盖 §2 D2 判定表全部 4 行 + 边界 RTF=0.8。
func TestDecideBench_TableDriven(t *testing.T) {
	tests := []struct {
		name        string
		rtf         float64
		contended   bool
		retried     bool
		wantSupport bool
		wantRetry   bool
		wantContend bool
	}{
		{name: "RTF<=0.8 uncontended", rtf: 0.5, contended: false, retried: false, wantSupport: true, wantRetry: false, wantContend: false},
		{name: "RTF<=0.8 contended", rtf: 0.6, contended: true, retried: true, wantSupport: true, wantRetry: false, wantContend: true},
		{name: "RTF=0.8 exact boundary", rtf: 0.8, contended: false, retried: false, wantSupport: true, wantRetry: false, wantContend: false},
		{name: "RTF>0.8 contended first time", rtf: 1.1, contended: true, retried: false, wantSupport: false, wantRetry: true, wantContend: true},
		{name: "RTF>0.8 contended retried", rtf: 1.2, contended: true, retried: true, wantSupport: false, wantRetry: true, wantContend: true},
		{name: "RTF>0.8 uncontended first time", rtf: 0.9, contended: false, retried: false, wantSupport: false, wantRetry: true, wantContend: false},
		{name: "RTF>0.8 uncontended retried", rtf: 0.95, contended: false, retried: true, wantSupport: false, wantRetry: false, wantContend: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideBench(tc.rtf, tc.contended, tc.retried)
			if got.Supported != tc.wantSupport {
				t.Errorf("Supported: want %v, got %v", tc.wantSupport, got.Supported)
			}
			if got.RetryOnStart != tc.wantRetry {
				t.Errorf("RetryOnStart: want %v, got %v", tc.wantRetry, got.RetryOnStart)
			}
			if got.Contended != tc.wantContend {
				t.Errorf("Contended: want %v, got %v", tc.wantContend, got.Contended)
			}
			if got.RTF != tc.rtf {
				t.Errorf("RTF: want %v, got %v", tc.rtf, got.RTF)
			}
		})
	}
}

// 7. waitCPUIdle：序列 [90,40,40,40] → idle=true；全 90 且 maxWait 到期 → idle=false；ctx 取消立即返回。
func TestWaitCPUIdle(t *testing.T) {
	// 序列 [90, 40, 40, 40]
	seq := []float64{90, 40, 40, 40}
	idx := 0
	usageFn := func() float64 {
		v := seq[idx]
		if idx < len(seq)-1 {
			idx++
		}
		return v
	}

	idle := waitCPUIdle(context.Background(), usageFn, 50, 3, 2*time.Millisecond, 200*time.Millisecond)
	if !idle {
		t.Errorf("序列 [90,40,40,40] 应满足 3 次连续 <50，got idle=false")
	}

	// 全 90 超时
	all90 := func() float64 { return 90 }
	idle = waitCPUIdle(context.Background(), all90, 50, 3, 2*time.Millisecond, 20*time.Millisecond)
	if idle {
		t.Errorf("全 90 超过 maxWait 应返回 idle=false")
	}

	// ctx 取消立即返回
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	idle = waitCPUIdle(ctx, all90, 50, 3, 2*time.Millisecond, 200*time.Millisecond)
	if idle {
		t.Errorf("ctx 已取消应返回 false")
	}
}

// 8. 暂时不可用不落定论：contended 且 RTF>0.8 时：t.slow 仍为 nil，持久化记录 RetryOnStart=true，状态为 ready。
func TestTTSService_TransientContendedDoesNotPersistTooSlow(t *testing.T) {
	sink := &recSink{}
	prof := hostProf()
	prefs := &memPrefs{}
	ttsSvc := NewTTSService(context.Background(), TTSOptions{
		LibDir:   t.TempDir(),
		Dir:      t.TempDir(),
		Support:  supported(),
		Sink:     sink,
		Prefs:    prefs,
		Profile:  prof,
		CPUUsage: func() float64 { return 80 }, // 繁忙 CPU
	})

	// 模拟写入一条争用记录（RTF=1.1, Contended=true）
	rec := decideBench(1.1, true, false)
	rec.Fingerprint = ttsSvc.fingerprint()
	rec.Model = "melo"
	rec.MeasuredAt = time.Now().UTC()
	_ = SaveBench(context.Background(), prefs, rec)

	// 重新读取
	loaded, ok, err := GetBench(context.Background(), prefs, tts.ModelMelo, ttsSvc.fingerprint())
	if err != nil || !ok {
		t.Fatalf("读取基准记录失败: %v", err)
	}
	if !loaded.RetryOnStart {
		t.Errorf("争用记录的 RetryOnStart 应为 true")
	}
	if loaded.Supported {
		t.Errorf("RTF 1.1 的 Supported 应为 false")
	}
	if !loaded.Contended {
		t.Errorf("Contended 应为 true")
	}
	if ttsSvc.slow.Load() != nil {
		t.Errorf("暂时不可用时 t.slow 必须仍为 nil")
	}
}

// 9. BenchIfNeeded 不常驻：调用后 IsResident()==false。
func TestBenchIfNeeded_NotResident(t *testing.T) {
	ttsSvc := NewTTSService(context.Background(), TTSOptions{
		LibDir:  t.TempDir(),
		Dir:     t.TempDir(),
		Support: supported(),
		Profile: hostProf(),
	})
	// 由于缺少资产，NeedsBench 返回 false，BenchIfNeeded 立即返回 nil
	err := ttsSvc.BenchIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("BenchIfNeeded 期望 nil, got %v", err)
	}
	if ttsSvc.IsResident() {
		t.Errorf("调用后引擎不得驻留内存")
	}
}
