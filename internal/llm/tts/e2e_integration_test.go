//go:build integration

package tts

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/llm/stt"
)

// 端到端：真实下载（经清单 sha256 校验）→ 真实 FFI 加载 → 合成 → 写 WAV。
// 手动运行：
//
//	POLARIS_TTS_E2E_LIB=<含 libsherpa-onnx-c-api 的目录> POLARIS_TTS_E2E_OUT=<输出目录> \
//	  go test -tags integration -run TestE2E_RealSynthesis -v -timeout 20m ./internal/llm/tts/
//
// 不进默认 CI：依赖外网与本机 sherpa 动态库。
func TestE2E_RealSynthesis(t *testing.T) {
	libDir, outDir := os.Getenv("POLARIS_TTS_E2E_LIB"), os.Getenv("POLARIS_TTS_E2E_OUT")
	if libDir == "" || outDir == "" {
		t.Skip("需设置 POLARIS_TTS_E2E_LIB / POLARIS_TTS_E2E_OUT")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	assetDir := filepath.Join(outDir, "assets")
	const text = "好的，我已经帮你查过了。另外，你昨天提交的 GitHub 代码审查，一共有 12 条评论。我说另外，请打开。"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := EnsureAssets(ctx, libDir, assetDir, http.DefaultClient, "", nil); err != nil {
		t.Fatalf("EnsureAssets: %v", err)
	}
	if miss := ModelMissing(ModelDir(assetDir)); miss != "" {
		t.Fatalf("安装后仍缺 %s", miss)
	}
	if err := LoadLibrary(filepath.Join(libDir, stt.LibName())); err != nil {
		t.Fatalf("LoadLibrary: %v", err)
	}
	eng, err := NewEngine(ModelDir(assetDir), Options{NumThreads: 4})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	start := time.Now()
	a, err := eng.Generate(ctx, text)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	el := time.Since(start)
	t.Logf("melo: 音频 %.2fs，合成 %.2fs，RTF %.3f，%d 字节", a.Duration.Seconds(), el.Seconds(), el.Seconds()/a.Duration.Seconds(), len(a.Data))
	if a.Duration < 5*time.Second {
		t.Fatalf("音频过短：%v", a.Duration)
	}
	if err := os.WriteFile(filepath.Join(outDir, "e2e-melo.wav"), a.Data, 0o644); err != nil {
		t.Fatal(err)
	}
}
