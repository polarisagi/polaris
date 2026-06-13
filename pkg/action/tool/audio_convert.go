package tool

import (
	"context"
	"os/exec"
	"time"
)

// ConvertToRawPCM 使用 ffmpeg 将音频转为 16kHz f32le 原始 PCM 流。
// 此函数属于 pkg/action 工具层，是 exec 调用的合法封装位置。
func ConvertToRawPCM(ctx context.Context, inPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inPath, "-f", "f32le", "-ac", "1", "-ar", "16000", "-")
	return cmd.Output()
}
