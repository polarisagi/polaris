package adapter

import (
	"context"
	"io"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/concurrent"
)

// closeOnCancel 在 ctx 取消时关闭 conn，返回的 stop 须在连接生命周期结束时调用。
//
// GR-10.2-002：gorilla/websocket 的 ReadMessage 不接受 context，长连接循环阻塞在
// 网络读上时 select 中的 <-ctx.Done() 永远轮不到——Manager.Stop/StopAll/热重载
// cancel 之后 goroutine 与连接一直挂着，直到对端发来下一条消息。关闭底层连接会让
// 阻塞中的 ReadMessage 立即返回错误，循环随之退出。
func closeOnCancel(ctx context.Context, conn io.Closer) (stop func()) {
	done := make(chan struct{})
	concurrent.SafeGo(context.WithoutCancel(ctx), "channel.adapter.close_on_cancel", func(context.Context) {
		select {
		case <-ctx.Done():
			if err := conn.Close(); err != nil {
				slog.Debug("channel: close conn on cancel failed", "err", err)
			}
		case <-done:
		}
	})
	return func() { close(done) }
}
