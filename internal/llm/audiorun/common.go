package audiorun

import (
	"fmt"
	"runtime"

	"github.com/polarisagi/polaris/internal/llm/audioassets"
)

// humanBytes 把字节数格式化为 MB/GB 文案（十进制，与归档体积的通行说法一致）。
func humanBytes(n int64) string {
	const mb = 1000 * 1000
	if n >= 1000*mb {
		return fmt.Sprintf("%.1fGB", float64(n)/float64(1000*mb))
	}
	return fmt.Sprintf("%dMB", (n+mb/2)/mb)
}

// sumSize 汇总清单项字节数。
func sumSize(assets []audioassets.Asset) int64 {
	var n int64
	for _, a := range assets {
		n += a.Size
	}
	return n
}

// downloadProgress 返回把下载进度翻译成状态快照的回调。
func downloadProgress(sink StatusSink) audioassets.ProgressFunc {
	return func(a audioassets.Asset, done, total int64) {
		sink.Publish(Status{
			State:     StateDownloading,
			Detail:    fmt.Sprintf("下载%s（%s）", a.Name, humanBytes(a.Size)),
			BytesDone: done, BytesTotal: total,
		})
	}
}

// dlopenHint 为动态库加载失败补充可操作的原因。
// sherpa-onnx 1.13.2 自带 libonnxruntime（arm64 与 x64）minos=15.5，macOS < 15.5 上 dlopen 必然失败，
// 给出原因而不是裸 dlopen 错误。
func dlopenHint() string {
	if runtime.GOOS == "darwin" {
		return "（需要 macOS ≥ 15.5：onnxruntime 1.24.4 minos）"
	}
	return ""
}

// nopSink 在调用方未提供 StatusSink 时使用，避免到处判 nil。
type nopSink struct{}

func (nopSink) Publish(Status) {}

// memMsg 是"空闲内存不足，此刻无法加载"的用户文案。
func memMsg(what string, freeMB, needMB uint64) string {
	return fmt.Sprintf("可用内存不足，暂时无法加载%s引擎（需要至少 %dMB 空闲，当前 %dMB）；释放内存后重试", what, needMB, freeMB)
}

// rtfText 格式化实时率，保留两位小数。
func rtfText(v float64) string { return fmt.Sprintf("%.2f", v) }
