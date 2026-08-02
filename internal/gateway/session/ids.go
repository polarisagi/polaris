package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newSessionID 生成新会话 ID（"sess_" + 16 字节随机十六进制）。
// 与 chat 包内同名小函数（sessions_helpers.go，供会话 CRUD 场景使用）各自
// 独立维护——生成格式无跨包一致性约束（消费方只需要一个不可预测的不透明
// 字符串），不同于 SessionIDPattern 那样的安全校验契约需要单一权威源。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b)
}
