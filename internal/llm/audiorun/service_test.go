package audiorun

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/stt"
)

type recSink struct {
	mu   sync.Mutex
	last Status
	all  []Status
}

func (r *recSink) Publish(s Status) {
	r.mu.Lock()
	r.last = s
	r.all = append(r.all, s)
	r.mu.Unlock()
}

func (r *recSink) get() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func supported() Capability { return Capability{Supported: true} }

func hostProf() HardwareProfile { return HostProfile(16 * gib) }

func TestSTTService_NotInstalledPublishedAtStartup(t *testing.T) {
	if !AudioSupport(hostProf()).STT.Supported {
		t.Skip("当前平台不支持 STT")
	}
	sink := &recSink{}
	s := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: supported(), Sink: sink})
	defer s.Close()
	st := sink.get()
	if st.State != StateNotInstalled || st.InstallSizeBytes <= 0 {
		t.Fatalf("空目录应为 not_installed 且带下载体积，got %+v", st)
	}
	// 请求侧：未安装 → NotReady(not_installed)，且不得触发任何加载。
	_, err := s.Transcribe([]float32{0}, 16000)
	nr, ok := AsNotReady(err)
	if !ok || nr.Code != CodeNotInstalled {
		t.Fatalf("应报 not_installed，got %v", err)
	}
	if s.IsResident() {
		t.Error("未安装时不得加载引擎")
	}
}

func TestSTTService_UnsupportedBlocksEverything(t *testing.T) {
	sink := &recSink{}
	cap := Capability{Reason: ReasonInsufficientRAM, Message: "语音输入 需要至少 2GB 内存 / 2 核"}
	s := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: cap, Sink: sink})
	defer s.Close()
	if st := sink.get(); st.State != StateUnsupported || st.Reason != ReasonInsufficientRAM {
		t.Fatalf("应为 unsupported，got %+v", st)
	}
	if _, err := s.Transcribe(nil, 16000); err == nil {
		t.Error("不支持时转写必须报错")
	} else if nr, ok := AsNotReady(err); !ok || nr.Code != CodeUnsupported {
		t.Errorf("应报 unsupported，got %v", err)
	}
	if started, err := s.Install(); started || err == nil {
		t.Errorf("不支持的机器不得开始安装，started=%v err=%v", started, err)
	}
}

func TestTTSService_UnsupportedAndGate(t *testing.T) {
	sink := &recSink{}
	cap := Capability{Reason: ReasonInsufficientCores, Message: "服务端朗读 需要至少 2GB 内存 / 2 核"}
	s := NewTTSService(context.Background(), TTSOptions{LibDir: t.TempDir(), Dir: t.TempDir(), Support: cap, Sink: sink, Profile: hostProf()})
	defer s.Close()
	if st := sink.get(); st.State != StateUnsupported || st.Reason != ReasonInsufficientCores {
		t.Fatalf("应为 unsupported，got %+v", st)
	}
	_, err := s.Generate(context.Background(), "你好")
	if nr, ok := AsNotReady(err); !ok || nr.Code != CodeUnsupported {
		t.Errorf("应报 unsupported，got %v", err)
	}
}

// 同指纹下持久化的"过慢"结论必须在启动时直接生效（不必再加载 600MB 模型重测）。
func TestTTSService_PersistedTooSlowRestored(t *testing.T) {
	prof := hostProf()
	prefs := &memPrefs{}
	if err := SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: Fingerprint(prof), RTF: 1.4, Supported: false, MeasuredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Sink: sink, Prefs: prefs, Profile: prof,
	})
	defer s.Close()
	if st := sink.get(); st.State != StateUnsupported || st.Reason != ReasonTooSlow || st.Model != "none" {
		t.Fatalf("应恢复为 unsupported(too_slow) 且模型为 none，got %+v", st)
	}
	if started, err := s.Install(); started || err == nil {
		t.Errorf("过慢机器不应再下载 330MB，started=%v err=%v", started, err)
	}
}

// 指纹不同的旧记录不得生效（换了机器）。
func TestTTSService_StaleFingerprintIgnored(t *testing.T) {
	prefs := &memPrefs{}
	if err := SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: "darwin/arm64;cores=1;ram_gib=1;model=melo", RTF: 9, Supported: false,
	}); err != nil {
		t.Fatal(err)
	}
	if !AudioSupport(hostProf()).TTS.Supported {
		t.Skip("当前平台不支持 TTS")
	}
	sink := &recSink{}
	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Sink: sink, Prefs: prefs, Profile: hostProf(),
	})
	defer s.Close()
	if st := sink.get(); st.State != StateNotInstalled {
		t.Errorf("旧指纹记录应被忽略，状态应为 not_installed，got %+v", st)
	}
}

// 内存不足：加载被拒，返回 insufficient_memory，资产状态保持 ready（瞬时状态，非 failed）。
func TestSTTService_InsufficientMemoryRefusesLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	touch := func(rel string) {
		t.Helper()
		writeFile(t, dir, rel)
	}
	if _, err := stubLib(); err != nil {
		t.Skip("平台无库清单")
	}
	touch(libFileName())
	if err := stt.WriteLibMarker(dir); err != nil {
		t.Fatal(err)
	}
	touch("model/model.onnx")
	touch("model/tokens.txt")
	touch("punct_model/model.onnx")

	sink := &recSink{}
	s := NewSTTService(context.Background(), STTOptions{
		Dir: dir, Support: supported(), Sink: sink,
		FreeMemMB: func() uint64 { return 100 }, // < 600MB
	})
	defer s.Close()
	if st := sink.get(); st.State != StateReady || st.Loaded {
		t.Fatalf("资产齐备应为 ready 且未加载，got %+v", st)
	}
	_, err := s.Transcribe([]float32{0}, 16000)
	nr, ok := AsNotReady(err)
	if !ok || nr.Code != CodeInsufficientMemory {
		t.Fatalf("应报 insufficient_memory，got %v", err)
	}
	if st := sink.get(); st.State != StateReady {
		t.Errorf("内存不足是瞬时状态，资产状态应保持 ready，got %+v", st)
	}
}
