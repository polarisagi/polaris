package audiorun

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
)

func newFallbackSvc(t *testing.T, prefs PrefStore, sink StatusSink, model tts.Model) *TTSService {
	t.Helper()
	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Sink: sink, Prefs: prefs,
		Profile: hostProf(), Model: model,
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Melo 在同指纹下已有定论 too_slow：重启后直接从 Matcha 起步，状态带模型名。
func TestTTSService_PersistedMeloSlowStartsOnMatcha(t *testing.T) {
	prof := hostProf()
	prefs := &memPrefs{}
	if err := SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: Fingerprint(prof, tts.ModelMelo), Model: "melo", RTF: 1.3, Supported: false, MeasuredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	s := newFallbackSvc(t, prefs, sink, tts.ModelMelo)
	if s.CurrentModel() != tts.ModelMatcha {
		t.Fatalf("应降到 matcha，got %s", s.CurrentModel())
	}
	if st := sink.get(); st.Model != "matcha" || st.State == StateUnsupported {
		t.Errorf("状态应为 matcha 且可用/待安装，got %+v", st)
	}
}

// 瞬时过慢（RetryOnStart）不采信：仍用 Melo。
func TestTTSService_RetryOnStartKeepsMelo(t *testing.T) {
	prof := hostProf()
	prefs := &memPrefs{}
	_ = SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: Fingerprint(prof, tts.ModelMelo), Model: "melo", RTF: 1.3, Supported: false, RetryOnStart: true,
	})
	s := newFallbackSvc(t, prefs, &recSink{}, tts.ModelMelo)
	if s.CurrentModel() != tts.ModelMelo {
		t.Errorf("RetryOnStart 记录不应触发降级，got %s", s.CurrentModel())
	}
}

// 降级链：Melo 定论过慢 → Matcha；Matcha 也过慢 → unsupported(too_slow)，模型 none。
func TestTTSService_FallbackChain(t *testing.T) {
	sink := &recSink{}
	s := newFallbackSvc(t, &memPrefs{}, sink, tts.ModelMelo)
	rec := BenchRecord{RTF: 1.2}

	err := s.onDefinitiveSlow(tts.ModelMelo, rec, "auto")
	if nr, ok := AsNotReady(err); !ok || nr.Code != CodeUnsupported {
		t.Fatalf("Melo 过慢应返回 NotReady(unsupported)，got %v", err)
	}
	if s.CurrentModel() != tts.ModelMatcha || s.slow.Load() != nil {
		t.Fatalf("应切到 matcha 且未整体判慢，model=%s", s.CurrentModel())
	}
	if st := sink.get(); st.State != StateNotInstalled || st.Model != "matcha" {
		t.Errorf("切换后应发布 matcha 待安装状态，got %+v", st)
	}

	err = s.onDefinitiveSlow(tts.ModelMatcha, rec, "auto")
	if nr, ok := AsNotReady(err); !ok || nr.Code != CodeUnsupported {
		t.Fatalf("Matcha 过慢应返回 NotReady(unsupported)，got %v", err)
	}
	if s.slow.Load() == nil {
		t.Fatal("降级链已尽应整体判慢")
	}
	if st := s.base(false); st.State != StateUnsupported || st.Reason != ReasonTooSlow || st.Model != TTSModelNone {
		t.Errorf("链尽应为 unsupported(too_slow)/none，got %+v", st)
	}
}

// 预置器：基准把模型从 Melo 降到 Matcha 后，要再跑一轮"安装 → 基准"；A 档不支持则一次都不调用。
func TestProvisioner_ContinuesWithFallbackModel(t *testing.T) {
	s := newFallbackSvc(t, &memPrefs{}, &recSink{}, tts.ModelMelo)
	var installs, benches atomic.Int32
	var models []tts.Model
	runProvisioner(context.Background(), ProvisionerOptions{
		STT: NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: Capability{Reason: ReasonInsufficientRAM}}),
		TTS: s,
		TTSInstallFunc: func(context.Context) (bool, error) {
			installs.Add(1)
			models = append(models, s.CurrentModel())
			return true, nil
		},
		TTSBenchFunc: func(context.Context) error {
			if benches.Add(1) == 1 {
				_ = s.onDefinitiveSlow(tts.ModelMelo, BenchRecord{RTF: 1.2}, "auto")
			}
			return nil
		},
	})
	if installs.Load() != 2 || benches.Load() != 2 {
		t.Fatalf("应跑两轮（melo、matcha），installs=%d benches=%d", installs.Load(), benches.Load())
	}
	if len(models) != 2 || models[0] != tts.ModelMelo || models[1] != tts.ModelMatcha {
		t.Errorf("两轮应依次处理 melo、matcha，got %v", models)
	}
}

func TestProvisioner_TierADoesNothing(t *testing.T) {
	a := AudioSupport(prof(1*gib, 1), "auto")
	sttSvc := NewSTTService(context.Background(), STTOptions{Dir: t.TempDir(), Support: a.STT})
	ttsSvc := NewTTSService(context.Background(), TTSOptions{LibDir: t.TempDir(), Dir: t.TempDir(), Support: a.TTS, Profile: hostProf()})
	defer ttsSvc.Close()
	var calls atomic.Int32
	count := func(context.Context) (bool, error) { calls.Add(1); return true, nil }
	runProvisioner(context.Background(), ProvisionerOptions{
		STT: sttSvc, TTS: ttsSvc, STTInstallFunc: count, TTSInstallFunc: count,
		TTSBenchFunc: func(context.Context) error { calls.Add(1); return nil },
	})
	if calls.Load() != 0 {
		t.Errorf("A 档不得预置任何资产，calls=%d", calls.Load())
	}
	if _, err := ttsSvc.Generate(context.Background(), "你好"); err == nil {
		t.Error("A 档 Generate 必须报 NotReady")
	}
}

// 旧 Kokoro 目录只在目录名恰为 kokoro 时才删，且失败不影响其余流程。
func TestRemoveLegacy(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "kokoro")
	if err := os.MkdirAll(filepath.Join(legacy, "model"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "tts")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewTTSService(context.Background(), TTSOptions{LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Profile: hostProf(), LegacyDir: other})
	defer s.Close()
	s.removeLegacy()
	if _, err := os.Stat(other); err != nil {
		t.Fatal("目录名不是 kokoro 的路径绝不能被删除")
	}

	s2 := NewTTSService(context.Background(), TTSOptions{LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Profile: hostProf(), LegacyDir: legacy})
	defer s2.Close()
	s2.removeLegacy()
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("kokoro 目录应被删除")
	}
	s2.removeLegacy() // 不存在时静默
}
