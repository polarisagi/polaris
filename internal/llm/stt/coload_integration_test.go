//go:build integration

package stt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/llm/embedonnx"
)

// 同进程双 ORT 共存（ADR-0110 修订三 / ADR-0109）：语音自带 ORT 1.28.2（sherpa 1.13.8），
// 向量化引擎独立加载 ORT 1.23.2。两种加载顺序都必须各自可用：
// 向量化的 OrtGetApiBase 版本是 1.23.2 的、sherpa 的转写正常出文字。
//
// 手动运行：
//
//	POLARIS_STT_DIR=<含 1.13.8 库 + model/ + punct_model/ 的目录> \
//	POLARIS_EMBED_ORT=<向量化的 libonnxruntime.dylib(1.23.2)> POLARIS_STT_WAV=<中文 wav> \
//	  go test -count=1 -tags integration -run TestCoLoadORT -v ./internal/llm/stt/
func TestCoLoadORT(t *testing.T) {
	dir, ortPath, wav := os.Getenv("POLARIS_STT_DIR"), os.Getenv("POLARIS_EMBED_ORT"), os.Getenv("POLARIS_STT_WAV")
	if dir == "" || ortPath == "" || wav == "" {
		t.Skip("需设置 POLARIS_STT_DIR / POLARIS_EMBED_ORT / POLARIS_STT_WAV")
	}
	for _, order := range []string{"sherpa-first", "embed-first"} {
		t.Run(order, func(t *testing.T) {
			// 同一进程内 libInst 只加载一次，所以两种顺序用子测试顺序验证：
			// 先跑的子测试决定本进程内的加载顺序；另一顺序请用 -run 单独再跑一次。
			if os.Getenv("POLARIS_COLOAD_ORDER") != order {
				t.Skip("本次进程只验证 POLARIS_COLOAD_ORDER=" + order)
			}
			load := func() {
				if err := LoadLibrary(filepath.Join(dir, LibName())); err != nil {
					t.Fatal(err)
				}
			}
			var api *embedonnx.ORTApi
			open := func() {
				a, err := embedonnx.OpenORT(ortPath)
				if err != nil {
					t.Fatal(err)
				}
				api = a
			}
			if order == "sherpa-first" {
				load()
				open()
			} else {
				open()
				load()
			}
			if api == nil {
				t.Fatal("向量化 ORT 未加载")
			}
			f, err := os.Open(wav)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			samples, rate, err := DecodeWAV(f)
			if err != nil {
				t.Fatal(err)
			}
			eng, err := NewEngine(ModelDir(dir), PunctModelDir(dir), "zh", 2, false)
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			res, err := eng.Transcribe(samples, rate)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: 转写=%q", order, res.Text)
			if !strings.Contains(res.Text, "已经帮你查过了") {
				t.Errorf("转写结果不含预期短语: %q", res.Text)
			}
		})
	}
}
