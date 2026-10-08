package audiorun

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
)

// seqProvider 按序返回不同耗时，验证"计时多次取最小"能滤掉争抢尖峰。
type seqProvider struct {
	delays []time.Duration
	i      int
}

func (s *seqProvider) Generate(_ context.Context, _ string) (tts.Audio, error) {
	d := s.delays[s.i]
	s.i++
	time.Sleep(d)
	return tts.Audio{Duration: 400 * time.Millisecond}, nil
}
func (s *seqProvider) Close() error { return nil }

func TestRunBench_TakesMinimumOfTimedRuns(t *testing.T) {
	// 预热 5ms（丢弃）、第一次计时被"抢占"到 300ms、第二次恢复到 100ms：应取后者 ≈0.25。
	p := &seqProvider{delays: []time.Duration{5 * time.Millisecond, 300 * time.Millisecond, 100 * time.Millisecond}}
	rtf, err := RunBench(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rtf > 0.5 {
		t.Errorf("应取最小 RTF≈0.25，got %.3f", rtf)
	}
}

func TestMaxTTSRTF_Is08(t *testing.T) {
	if MaxTTSRTF != 0.8 {
		t.Errorf("阈值应为 0.8（预取播放只需 RTF<1 并留余量），got %v", MaxTTSRTF)
	}
}

// retry_on_start 的 unsupported 记录：启动时不采信（状态不是 unsupported），
// 且 supported 记录同指纹继续复用。
func TestTTSService_RetryOnStartNotTrustedAtBoot(t *testing.T) {
	prof := hostProf()
	prefs := &memPrefs{}
	if err := SaveBench(context.Background(), prefs, BenchRecord{
		Fingerprint: Fingerprint(prof, tts.ModelMelo), Model: "melo", RTF: 1.07, Supported: false, RetryOnStart: true, MeasuredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	s := NewTTSService(context.Background(), TTSOptions{
		LibDir: t.TempDir(), Dir: t.TempDir(), Support: supported(), Sink: sink, Prefs: prefs, Profile: prof,
	})
	defer s.Close()
	if st := sink.get(); st.State == StateUnsupported {
		t.Fatalf("待重测的 unsupported 不应在启动时生效，got %+v", st)
	}
}
