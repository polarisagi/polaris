package builtin

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
	// 注意：这里保留原生 exec.CommandContext，不接入 Rust V2 沙箱（runSandboxedArgv）。
	// 原因：Rust 侧沙箱执行路径（run_with_timeout）会将 stdout 和 stderr 合并为单一文本流，
	// 而 ffmpeg 的二进制 PCM 音频流必须保持纯净（仅取 stdout）。如果走沙箱，合并的 stderr
	// 文本会污染二进制流，导致下游 STT/TTS 模块解析失败。此外，inPath 是内部可信路径，
	// 命令不包含外部输入拼接，本身 shell 注入风险极低。
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inPath, "-f", "f32le", "-ac", "1", "-ar", "16000", "-")
	return cmd.Output()
}
