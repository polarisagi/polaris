package chat

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// saveMessageRetryAttempts：SaveMessage 同步直写 chat_messages 失败时的有限
// 重试次数（GD-13-004 复核修复）。SQLite 单写者 + busy_timeout=5000ms 下绝
// 大多数瞬时锁争用已在驱动层被吸收，这里的重试面向剩余的极端瞬时故障（磁盘
// 短暂繁忙等）；重试耗尽后转 outbox 异步兜底，不在请求路径无限阻塞。
const saveMessageRetryAttempts = 3

// saveMessageRetryBackoff 返回第 attempt（0-based）次重试前的等待时长。
func saveMessageRetryBackoff(attempt int) time.Duration {
	backoffs := [saveMessageRetryAttempts]time.Duration{
		50 * time.Millisecond, 150 * time.Millisecond, 450 * time.Millisecond,
	}
	if attempt < 0 || attempt >= len(backoffs) {
		return backoffs[len(backoffs)-1]
	}
	return backoffs[attempt]
}

// ============================================================================
// 会话辅助方法：创建/加载/保存消息、标题更新、心跳、ID 生成（R7 拆分自
// sessions.go）。CRUD HTTP 处理器见 sessions.go，全文搜索见 sessions_search.go。
// ============================================================================

// newMessageDedupeKey 生成消息幂等键：会话/角色前缀便于人工排查 + 随机后缀
// 保证唯一性（熵池耗尽时降级为纳秒时间戳，与 newSessionID 一致的降级策略）。
func newMessageDedupeKey(sessionID, role string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s:%s:%d", sessionID, role, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s:%s:%s", sessionID, role, hex.EncodeToString(b))
}

// newSessionID 生成 16 字节随机 hex ID。
// 熵池耗尽时降级为纳秒时间戳，确保唯一性（不使用固定零值，防止 session 碰撞）。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b)
}

// truncate 截断字节，防止错误消息过长写入 SSE。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
