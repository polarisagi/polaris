package guard

import (
	"crypto/rand"
	"encoding/hex"
)

// ============================================================================
// 安全随机数取用（2026-08-06：errcheck 门控接入 internal/security 后收敛）
//
// 此前 PII 脱敏器与令牌金库共有 3 处 `_, _ = rand.Read(b)`。crypto/rand.Read
// 自 Go 1.24 起契约上不返回错误（系统熵源不可用时其内部直接 panic），所以那
// 3 处**行为上**是安全的——但写成静默丢弃，读者无从区分"这里不可能出错"与
// "这里忘了处理错误"，而后者在这个位置的后果相当严重：
//
//	rand.Read 若真的失败而返回全零缓冲，PIITokenVault.TokenizeForTask 生成的
//	shortID 会恒为 "00000000"，同一 task 内**所有** PII 令牌塌缩成同一个 key，
//	v.tokens[taskID][token] = original 互相覆盖；Restore 时每个占位符都会被
//	还原成最后写入的那一个值——PII 交叉污染。
//
// 因此这里把前提显式写出来：取不到熵就 fail-fast，绝不用可预测字节继续生成
// 安全标识符。
// ============================================================================

// secureRandomBytes 返回 n 字节密码学随机数。
//
// panic 而非返回 error 是刻意的，且与"库函数不应 panic"的一般原则不冲突：
// 这里不是调用方传参错误（那类必须返回 error，见 benchmark.FetchDataset），
// 而是系统熵源整体不可用——一个进程无法继续提供任何安全保证的状态，
// 与 Go 标准库 crypto/rand 自身在同种情形下的处置一致。
func secureRandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("guard: crypto/rand unavailable, refusing to emit predictable security identifiers: " + err.Error())
	}
	return b
}

// secureRandomHex 返回 n 字节随机数的小写十六进制表示（长度 2n）。
//
// 十六进制而非 crypto/rand.Text()：PII 令牌格式受 tokenPattern
// `⟦PII:[0-9a-f]{8}⟧` 约束，必须是小写 hex；Text() 产出的是大写 base32。
func secureRandomHex(n int) string {
	return hex.EncodeToString(secureRandomBytes(n))
}
