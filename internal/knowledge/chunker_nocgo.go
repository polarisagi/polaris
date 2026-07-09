//go:build !cgo

package knowledge

import (
	"strings"
)

// CodeChunker 按 func 和 class 边界进行分块。
// [Task 15] 当 CGO 被禁用时（例如跨平台交叉编译），使用回退的基于字符串前缀匹配的逻辑。
type CodeChunker struct{}

func (c *CodeChunker) Chunk(content, sourceType string) []string {
	return c.fallbackChunk(content)
}

// fallbackChunk 原有的字符串前缀匹配分块逻辑
func (c *CodeChunker) fallbackChunk(content string) []string {
	var finalChunks []string
	lines := strings.Split(content, "\n")
	var current strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "class ") {
			if current.Len() > 0 {
				p := strings.TrimSpace(current.String())
				if p != "" {
					finalChunks = append(finalChunks, splitByLimit(p)...)
				}
				current.Reset()
			}
		}
		current.WriteString(line + "\n")
	}
	if current.Len() > 0 {
		p := strings.TrimSpace(current.String())
		if p != "" {
			finalChunks = append(finalChunks, splitByLimit(p)...)
		}
	}
	return finalChunks
}
