//go:build nettest

package tts

import (
	"context"
	"testing"
	"time"
)

// Edge TTS 协议契约：真实握手（含 Sec-MS-GEC）+ 合成"你好"，断言 MIME=audio/mpeg 且字节 > 1000。
// 微软升版本/改协议时这里最先红；依赖外网，不进默认 CI，改动 Edge 协议必须手动 make audio-nettest。
func TestNet_EdgeSynthesizeNiHao(t *testing.T) {
	p := NewEdgeProvider("", "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := p.Generate(ctx, "你好")
	if err != nil {
		t.Fatalf("edge generate: %v", err)
	}
	if a.MIME != MIMEMP3 {
		t.Errorf("MIME = %q, want %q", a.MIME, MIMEMP3)
	}
	if len(a.Data) <= 1000 {
		t.Errorf("audio too small: %d bytes", len(a.Data))
	}
}
