package audiorun

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// A 档：预置器一次安装/基准都不调用；Generate 报 NotReady。
func TestProvisioner_TierADoesNothing(t *testing.T) {
	a := AudioSupport(prof(1*gib, 1))
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

// Melo 基准定论 too_slow：没有更轻的服务端模型，直接 unsupported(too_slow) 回前端系统语音，模型名为 none。
func TestTTSService_MeloTooSlowGoesToSystemVoice(t *testing.T) {
	sink := &recSink{}
	s := NewTTSService(context.Background(), TTSOptions{LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Sink: sink, Profile: hostProf()})
	defer s.Close()
	if st := sink.get(); st.Model != "melo" {
		t.Errorf("初始状态应带模型名 melo，got %+v", st)
	}
	s.slow.Store(&BenchRecord{RTF: 1.2})
	if st := s.base(false); st.State != StateUnsupported || st.Reason != ReasonTooSlow || st.Model != "none" {
		t.Errorf("应为 unsupported(too_slow)/none，got %+v", st)
	}
}

// 旧 Kokoro / Matcha 目录只在目录名恰为 kokoro / matcha 时才删，其他路径绝不动。
func TestRemoveLegacy(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Join(p, "model"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kokoro, matcha, other := mk("kokoro"), mk("tts/matcha"), mk("tts/melo")

	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Profile: hostProf(),
		LegacyDirs: []string{kokoro, matcha, other, ""},
	})
	defer s.Close()
	s.removeLegacy()
	for _, p := range []string{kokoro, matcha} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s 应被删除", p)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("目录名不是 kokoro/matcha 的路径绝不能被删除")
	}
	s.removeLegacy() // 已不存在时静默
}
