//go:build integration

package audiorun

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
)

// 代理系数的同管线标定（ADR-0110 修订三）：同一进程、同一时刻、交替测 Go 管线的
// SenseVoice RTF（MeasureSTTRTF）与 Go 管线的 Melo 真实基准（RunBench：预热 1 + 计时 2 取最小），
// 逐线程多轮算 ratio = meloRTF / sttRTF。系数 = 全部轮次 ratio 最小值 × 0.9。
//
// 手动运行（需外网下载 Melo 约 167MB 到 POLARIS_CALIB_DIR，机器应尽量空闲）：
//
//	POLARIS_STT_DIR=~/.polaris/models/sensevoice POLARIS_CALIB_DIR=~/tts-ab/go-calib \
//	  go test -count=1 -tags integration -run TestCalibrateProxyRatio -v -timeout 30m ./internal/llm/audiorun/
func TestCalibrateProxyRatio(t *testing.T) {
	home, _ := os.UserHomeDir()
	sttDir := os.Getenv("POLARIS_STT_DIR")
	if sttDir == "" {
		sttDir = filepath.Join(home, ".polaris", "models", "sensevoice")
	}
	calDir := os.Getenv("POLARIS_CALIB_DIR")
	if calDir == "" {
		calDir = filepath.Join(home, "tts-ab", "go-calib")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	if err := tts.EnsureAssets(ctx, sttDir, calDir, http.DefaultClient, "", nil); err != nil {
		t.Skipf("无法准备 Melo 资产: %v", err)
	}
	if err := stt.LoadLibrary(filepath.Join(sttDir, stt.LibName())); err != nil {
		t.Skip(err)
	}
	if err := tts.LoadLibrary(filepath.Join(sttDir, stt.LibName())); err != nil {
		t.Skip(err)
	}
	const rounds = 3
	minRatio := 1e9
	for _, th := range []int{1, 2, 4} {
		sttEng, err := stt.NewEngine(stt.ModelDir(sttDir), stt.PunctModelDir(sttDir), "zh", th, false)
		if err != nil {
			t.Skip(err)
		}
		meloEng, err := tts.NewEngine(tts.ModelDir(calDir), tts.Options{NumThreads: th})
		if err != nil {
			sttEng.Close()
			t.Fatal(err)
		}
		for r := 1; r <= rounds; r++ {
			sttRTF, err := MeasureSTTRTF(func(s []float32, rate int) error {
				_, e := sttEng.Transcribe(s, rate)
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			meloRTF, err := RunBench(ctx, meloEng)
			if err != nil {
				t.Fatal(err)
			}
			ratio := meloRTF / sttRTF
			if ratio < minRatio {
				minRatio = ratio
			}
			t.Logf("threads=%d round=%d sttRTF=%.4f meloRTF=%.4f ratio=%.2f", th, r, sttRTF, meloRTF, ratio)
		}
		sttEng.Close()
		_ = meloEng.Close()
	}
	t.Logf("min ratio=%.2f  coefficient(min*0.9)=%.2f  (当前 proxyRatio=%.2f)", minRatio, minRatio*0.9, proxyRatio)
}
