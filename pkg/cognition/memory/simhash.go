package memory

import (
	"math/bits"
	"strings"
)

// Simhash — 64-bit Simhash 指纹 + 汉明距离。
// 架构文档: docs/arch/M05-Memory-System.md §7.2
//
// 用途: embedding API 不可用时的降级检索路径。
// 算法:
//   1. 分词（空格 + 标点切割）
//   2. 每个词 FNV-64 哈希
//   3. 64 位向量累加权重（哈希 bit=1 → +1，=0 → -1）
//   4. 正 → 1，负 → 0，构成 64-bit 指纹
//   5. 汉明距离 = bits.OnesCount64(a ^ b)，≤ 8 视为相似

// Fingerprint 64-bit Simhash 指纹。
type Fingerprint uint64

// SimhashOf 计算文本的 Simhash 指纹。
func SimhashOf(text string) Fingerprint {
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return 0
	}

	// 64 维权重累加向量
	var v [64]int
	for _, tok := range tokens {
		h := fnv64(tok)
		for i := 0; i < 64; i++ {
			if (h>>uint(i))&1 == 1 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}

	var fp Fingerprint
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			fp |= 1 << uint(i)
		}
	}
	return fp
}

// Hamming 计算两个指纹的汉明距离。
func (a Fingerprint) Hamming(b Fingerprint) int {
	return bits.OnesCount64(uint64(a ^ b))
}

// IsSimilar 判断两个文本是否相似（汉明距离 ≤ 8）。
func IsSimilar(a, b Fingerprint) bool {
	return a.Hamming(b) <= 8
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// tokenize 按空格和标点分词（不依赖外部库，Tier 0 纯 Go 实现）。
func tokenize(text string) []string { //nolint:gocyclo
	var tokens []string
	var current strings.Builder
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' ||
			r == ',' || r == '.' || r == '!' || r == '?' ||
			r == '；' || r == '，' || r == '。' || r == '！' || r == '？' {
			if current.Len() > 0 {
				tok := strings.ToLower(current.String())
				if !isStopWord(tok) {
					tokens = append(tokens, tok)
				}
				current.Reset()
			}
		} else {
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tok := strings.ToLower(current.String())
		if !isStopWord(tok) {
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

// fnv64 FNV-1a 64-bit 哈希（零依赖实现）。
func fnv64(s string) uint64 {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// isStopWord 过滤英文停用词（减少噪音 bit）。
func isStopWord(w string) bool {
	switch w {
	case "a", "an", "the", "is", "are", "was", "were",
		"in", "on", "at", "to", "for", "of", "and", "or", "but",
		"it", "this", "that", "i", "you", "we", "he", "she",
		"的", "了", "是", "在", "和", "与", "或", "不", "也":
		return true
	}
	return false
}
