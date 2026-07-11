package prompt

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewRandomBoundary 生成一对带有 16 字节随机十六进制后缀的边界符，
// 用于包裹不可信内容，防止 LLM 被固定边界符逃逸攻击。
// 例如：返回 ("[CODE_START_1a2b...]", "[CODE_END_1a2b...]")
func NewRandomBoundary() (start string, end string) {
	b := make([]byte, 16)
	// crypto/rand.Read 一定会返回 16 字节，或者 panic（极其罕见的 OS 熵池耗尽）
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b)

	start = fmt.Sprintf("[CODE_START_%s]", suffix)
	end = fmt.Sprintf("[CODE_END_%s]", suffix)
	return start, end
}
