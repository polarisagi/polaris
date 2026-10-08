//go:build integration

package audiorun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 代理测速真机探针：用本机已安装的 SenseVoice 在 1/2/4 线程下测 RTF 并外推 Melo。
// 手动运行（不进默认 CI，依赖本机 sherpa 动态库与 SenseVoice 资产）：
//
//	POLARIS_STT_DIR=~/.polaris/models/sensevoice \
//	  go test -tags integration -run TestProxyProbe_RealSenseVoice -v ./internal/llm/audiorun/
func TestProxyProbe_RealSenseVoice(t *testing.T) {
	dir := os.Getenv("POLARIS_STT_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".polaris", "models", "sensevoice")
	}
	svc := NewSTTService(context.Background(), STTOptions{Dir: dir, Support: supported()})
	defer svc.Close()
	for _, threads := range []int{1, 2, 4} {
		rtf, ok := svc.ProbeRTF(context.Background(), threads)
		if !ok {
			t.Skipf("STT 资产不可用于探针: %s", dir)
		}
		pred := proxyPredicted(rtf)
		if threads >= 2 && pred > proxyMaxPredicted {
			t.Errorf("M1 自检失败：threads=%d predicted=%.3f 被判过慢，系数错误", threads, pred)
		}
		t.Logf("threads=%d sttRTF=%.4f predicted=%.3f (放行上限 %.1f, 过慢=%v)", threads, rtf, pred, proxyMaxPredicted, pred > proxyMaxPredicted)
	}
}
