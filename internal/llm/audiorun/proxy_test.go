package audiorun

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
)

// 预测值边界（系数 11.2，放行线 sttRTF≈0.08929）：0.0892×11.2=0.9990 放行，0.0893×11.2=1.0002 判慢（ADR-0110 修订三）。
func TestDecideProxy_Boundary(t *testing.T) {
	cases := []struct {
		sttRTF    float64
		supported bool
	}{
		{0.0391, true}, {0.0892, true}, {0.0893, false}, {0.5, false},
	}
	for _, c := range cases {
		rec, ok := decideProxy(c.sttRTF, false)
		if !ok {
			t.Fatalf("sttRTF=%v 非争用必须有结论", c.sttRTF)
		}
		if rec.Supported != c.supported || rec.Method != MethodProxy || rec.RetryOnStart {
			t.Errorf("sttRTF=%v: got %+v, want supported=%v method=proxy retry=false", c.sttRTF, rec, c.supported)
		}
		if rec.STTRTF != c.sttRTF || rec.Predicted != c.sttRTF*proxyRatio {
			t.Errorf("sttRTF=%v: 记录应带 stt_rtf 与 predicted，got %+v", c.sttRTF, rec)
		}
	}
}

// 争用下的测量不可信：无结论，哪怕 RTF 很大也不得据此拒绝下载。
func TestDecideProxy_ContendedNoConclusion(t *testing.T) {
	if _, ok := decideProxy(0.5, true); ok {
		t.Error("CPU 争用时不得给出结论")
	}
}

func TestMeasureSTTRTF_WarmupPlusMinOfTimed(t *testing.T) {
	var calls int
	delays := []time.Duration{0, 60 * time.Millisecond, 20 * time.Millisecond, 60 * time.Millisecond}
	rtf, err := MeasureSTTRTF(func(samples []float32, rate int) error {
		if len(samples) != 160000 || rate != 16000 {
			t.Errorf("输入应为 10s/16kHz，got %d@%d", len(samples), rate)
		}
		time.Sleep(delays[calls])
		calls++
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("应 1 预热 + 3 计时，calls=%d err=%v", calls, err)
	}
	if rtf < 0.002 || rtf > 0.005 {
		t.Errorf("应取最小 ≈20ms/10s=0.002，got %v", rtf)
	}
	if _, err := MeasureSTTRTF(func([]float32, int) error { return errors.New("boom") }); err == nil {
		t.Error("转写失败必须返回错误")
	}
}

func TestProxyNoise_Deterministic(t *testing.T) {
	a, b := proxyNoise(), proxyNoise()
	for i := range a {
		if a[i] != b[i] || a[i] > 0.01 || a[i] < -0.01 {
			t.Fatalf("噪声必须固定种子且幅度 ≤0.01，i=%d", i)
		}
	}
}

// netCount 统计出站 HTTP 请求并一律失败：下载被触发即可观测，且不会真的联网。
type netCount struct{ n atomic.Int32 }

func (c *netCount) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("offline in test")
}

func proxySvc(t *testing.T, probe ProxyProbeFunc, cpu float64, prefs PrefStore) (*TTSService, *netCount, *recSink) {
	t.Helper()
	nc := &netCount{}
	sink := &recSink{}
	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Sink: sink, Prefs: prefs,
		Profile: hostProf(), HTTPClient: &http.Client{Transport: nc},
		CPUUsage: func() float64 { return cpu }, ProxyProbe: probe,
	})
	t.Cleanup(func() { _ = s.Close() })
	return s, nc, sink
}

// 代理判慢：不下载、落库 method=proxy、状态 unsupported/too_slow，预置器随后跳过。
func TestProxy_TooSlowSkipsDownload(t *testing.T) {
	prefs := &memPrefs{}
	s, nc, sink := proxySvc(t, func(context.Context, int) (float64, bool) { return 0.3, true }, 0, prefs)
	ran, err := s.InstallBlocking(context.Background())
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if nc.n.Load() != 0 {
		t.Error("代理判慢时不得发起任何下载请求")
	}
	if st := sink.get(); st.State != StateUnsupported || st.Reason != ReasonTooSlow || st.Model != "none" {
		t.Errorf("状态应为 unsupported/too_slow/none，got %+v", st)
	}
	rec, ok, _ := GetBench(context.Background(), prefs, s.fingerprint())
	if !ok || rec.Method != MethodProxy || rec.Supported || rec.STTRTF != 0.3 {
		t.Errorf("应落库代理记录，got %+v ok=%v", rec, ok)
	}
	if ran, _ := s.InstallBlocking(context.Background()); ran {
		t.Error("已判慢，后台预置器不得再安装")
	}
	// 重启后同指纹的代理结论直接生效。
	s2 := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Prefs: prefs, Profile: hostProf(),
	})
	defer s2.Close()
	if s2.slow.Load() == nil {
		t.Error("代理判慢记录应在重启后恢复")
	}
}

// 代理放行：继续下载（这里下载被拒绝，但请求已发出）。
func TestProxy_PassProceedsToDownload(t *testing.T) {
	s, nc, _ := proxySvc(t, func(context.Context, int) (float64, bool) { return 0.05, true }, 0, &memPrefs{})
	_, _ = s.InstallBlocking(context.Background())
	if nc.n.Load() == 0 || s.slow.Load() != nil {
		t.Errorf("放行应继续下载且不置 slow，requests=%d slow=%v", nc.n.Load(), s.slow.Load())
	}
}

// 争用：无结论，走原路径下载，且不落库。
// 后台自动路径会先等 CPU 空闲（最多 10 分钟），所以 CPU 在探针运行期间才变忙：测前空闲、测后繁忙。
func TestProxy_ContendedFallsBackToDownload(t *testing.T) {
	prefs := &memPrefs{}
	var busy atomic.Bool
	nc := &netCount{}
	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Prefs: prefs, Profile: hostProf(),
		HTTPClient: &http.Client{Transport: nc},
		CPUUsage: func() float64 {
			if busy.Load() {
				return 90
			}
			return 0
		},
		ProxyProbe: func(context.Context, int) (float64, bool) { busy.Store(true); return 0.5, true },
	})
	defer s.Close()
	_, _ = s.InstallBlocking(context.Background())
	if nc.n.Load() == 0 || s.slow.Load() != nil {
		t.Errorf("争用无结论应继续下载，requests=%d", nc.n.Load())
	}
	if _, ok, _ := GetBench(context.Background(), prefs, s.fingerprint()); ok {
		t.Error("无结论不得落库")
	}
}

// STT 资产缺失/加载失败（探针 ok=false）：跳过代理，走原路径。
func TestProxy_STTUnavailableSkipsProxy(t *testing.T) {
	s, nc, _ := proxySvc(t, func(context.Context, int) (float64, bool) { return 0, false }, 0, &memPrefs{})
	_, _ = s.InstallBlocking(context.Background())
	if nc.n.Load() == 0 || s.slow.Load() != nil {
		t.Errorf("探针不可用应跳过代理并下载，requests=%d", nc.n.Load())
	}
	// 真实 STTService：资产缺失时探针必须报 ok=false。
	stts := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: supported()})
	defer stts.Close()
	if _, ok := stts.ProbeRTF(context.Background(), 2); ok {
		t.Error("STT 资产缺失时 ProbeRTF 必须 ok=false")
	}
}

// 手动安装无视代理结论：不调探针、清除代理 slow、发起下载。
func TestProxy_ManualInstallBypasses(t *testing.T) {
	var probed atomic.Int32
	s, nc, _ := proxySvc(t, func(context.Context, int) (float64, bool) { probed.Add(1); return 0.3, true }, 0, &memPrefs{})
	s.slow.Store(&BenchRecord{Method: MethodProxy, Predicted: 1.3, RTF: 1.3})
	started, err := s.Install()
	if err != nil || !started {
		t.Fatalf("手动安装应被允许，started=%v err=%v", started, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (nc.n.Load() == 0 || s.installing.Load()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if nc.n.Load() == 0 {
		t.Error("手动安装必须发起下载")
	}
	if probed.Load() != 0 {
		t.Error("手动安装不得再跑代理测速")
	}
	if s.slow.Load() != nil {
		t.Error("手动安装应清除代理的 slow 结论")
	}
}

// 真实基准的定论仍拒绝手动安装（已测过，资产已回收）。
func TestProxy_RealVerdictStillRefusesInstall(t *testing.T) {
	s, _, _ := proxySvc(t, nil, 0, &memPrefs{})
	s.slow.Store(&BenchRecord{Method: MethodReal, RTF: 1.2})
	if started, err := s.Install(); started || err == nil {
		t.Errorf("真实定论应拒绝安装，started=%v err=%v", started, err)
	}
}

func seedMelo(t *testing.T, s *TTSService) string {
	t.Helper()
	dir := tts.ModelDir(s.o.Dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "model.onnx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// 定论过慢（非争用、已重测）：置 slow 并删除 models/tts/melo；状态保持 unsupported/too_slow。
func TestReclaim_DefinitiveVerdictDeletesDir(t *testing.T) {
	s, _, _ := proxySvc(t, nil, 0, &memPrefs{})
	dir := seedMelo(t, s)
	rec := decideBench(1.2, false, true)
	if rec.RetryOnStart || rec.Supported {
		t.Fatalf("前置：应为定论过慢 %+v", rec)
	}
	err := s.finishSlow(rec)
	if nr, ok := AsNotReady(err); !ok || nr.Code != CodeUnsupported {
		t.Errorf("应报 unsupported，got %v", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Error("定论过慢后 melo 目录必须被回收")
	}
	if s.slow.Load() == nil {
		t.Error("应置 slow")
	}
	if st := s.base(false); st.State != StateUnsupported || st.Reason != ReasonTooSlow {
		t.Errorf("回收后状态应保持 too_slow，got %+v", st)
	}
	// slow 已置位后并发 Generate/加载不得再碰资产。
	if _, lerr := s.loadEngine(context.Background()); lerr == nil {
		t.Error("slow 后 loadEngine 必须拒绝")
	}
}

// 争用造成的不可用（RetryOnStart=true）：绝不删除资产，也不置 slow。
func TestReclaim_ContentionDoesNotDelete(t *testing.T) {
	s, _, _ := proxySvc(t, nil, 0, &memPrefs{})
	dir := seedMelo(t, s)
	for _, rec := range []BenchRecord{decideBench(1.2, true, true), decideBench(1.2, false, false)} {
		if !rec.RetryOnStart {
			t.Fatalf("前置：应为待重测 %+v", rec)
		}
		_ = s.finishSlow(rec)
		if _, serr := os.Stat(dir); serr != nil {
			t.Fatalf("争用/首次慢结果不得删除资产: %v", serr)
		}
		if s.slow.Load() != nil {
			t.Error("待重测不得置 slow")
		}
	}
}

// 回收只删目录名恰为 melo 的路径。
func TestReclaim_OnlyMeloDirName(t *testing.T) {
	s, _, _ := proxySvc(t, nil, 0, &memPrefs{})
	other := filepath.Join(s.o.Dir, "keep")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	s.reclaimAssets()
	if _, err := os.Stat(other); err != nil {
		t.Error("不得误删兄弟目录")
	}
}

// 启动恢复：真实定论 too_slow 且 melo 目录仍在 → 回收；代理记录不触碰；待重测记录不删。
func TestReclaim_RestoreBenchAtStartup(t *testing.T) {
	cases := []struct {
		name       string
		rec        BenchRecord
		wantExists bool
	}{
		{"真实定论回收", BenchRecord{Method: MethodReal, RTF: 1.2}, false},
		{"旧版无 method 的定论也回收", BenchRecord{RTF: 1.2}, false},
		{"代理记录不删", BenchRecord{Method: MethodProxy, RTF: 1.3, Predicted: 1.3}, true},
		{"待重测不删", BenchRecord{Method: MethodReal, RTF: 1.2, RetryOnStart: true}, true},
	}
	for _, c := range cases {
		prefs := &memPrefs{}
		c.rec.Fingerprint = Fingerprint(hostProf())
		if err := SaveBench(context.Background(), prefs, c.rec); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		dir := filepath.Join(root, tts.ModelName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		s := NewTTSService(context.Background(), TTSOptions{
			LibDir: t.TempDir(), Dir: root, Support: supported(), Prefs: prefs, Profile: hostProf(),
		})
		_ = s.Close()
		if _, err := os.Stat(dir); (err == nil) != c.wantExists {
			t.Errorf("%s: 目录存在=%v, want %v", c.name, err == nil, c.wantExists)
		}
	}
}
