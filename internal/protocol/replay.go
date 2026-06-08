package protocol

import "sync/atomic"

var isReplaying atomic.Bool

// SetReplayMode 设置全局回放模式标志。
func SetReplayMode(replaying bool) {
	isReplaying.Store(replaying)
}

// IsReplaying 返回当前是否处于全局回放模式。
// M4、M5、M11 在执行 EmitEvent、ToolCall、Outbox 投递等外部副作用前，
// 必须检查此标志，若为 true 则物理切断副作用。
func IsReplaying() bool {
	return isReplaying.Load()
}
