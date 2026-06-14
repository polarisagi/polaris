package knowledge

import (
	"strings"
)

// ChunkStrategy 定义了文档分块策略（consumer-side 接口）。
type ChunkStrategy interface {
	// Chunk 将原始内容切分为多个小块。
	Chunk(content, sourceType string) []string
}

const chunkMaxBytes = 1500

// splitByLimit 辅助函数，确保每个块不超过 chunkMaxBytes。
// 若超出，则尝试按句子（例如 ". "）分割，否则强制截断。
func splitByLimit(text string) []string {
	if len(text) <= chunkMaxBytes {
		return []string{text}
	}

	var chunks []string
	sentences := strings.Split(text, ". ")
	var current strings.Builder

	for i, s := range sentences {
		addDot := ""
		if i < len(sentences)-1 {
			addDot = ". "
		}

		if current.Len()+len(s)+len(addDot) > chunkMaxBytes {
			if current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
			}
			chunks, s = handleLongSentence(chunks, s)
			if len(s) > 0 {
				current.WriteString(s + addDot)
			}
		} else {
			current.WriteString(s + addDot)
		}
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

func handleLongSentence(chunks []string, s string) ([]string, string) {
	if len(s) <= chunkMaxBytes {
		return chunks, s
	}
	for len(s) > chunkMaxBytes {
		chunks = append(chunks, s[:chunkMaxBytes])
		s = s[chunkMaxBytes:]
	}
	return chunks, s
}

// PlainTextChunker 按双换行符粗粒度分块（Fallback）。
type PlainTextChunker struct{}

func (c *PlainTextChunker) Chunk(content, sourceType string) []string {
	var finalChunks []string
	parts := strings.Split(content, "\n\n")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		finalChunks = append(finalChunks, splitByLimit(p)...)
	}
	return finalChunks
}

// MarkdownChunker 识别 ## / ### 标题边界进行分块。
type MarkdownChunker struct{}

func (c *MarkdownChunker) Chunk(content, sourceType string) []string {
	var finalChunks []string
	lines := strings.Split(content, "\n")
	var current strings.Builder

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
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

// CodeChunker 按 func 和 class 边界进行分块。
type CodeChunker struct{}

func (c *CodeChunker) Chunk(content, sourceType string) []string {
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

// DefaultChunker 策略路由入口，根据 sourceType 返回对应的分块结果。
type DefaultChunker struct{}

func (c *DefaultChunker) Chunk(content, sourceType string) []string {
	var strategy ChunkStrategy
	switch sourceType {
	case "md", "markdown":
		strategy = &MarkdownChunker{}
	case "go", "py", "python", "js", "ts", "javascript", "typescript", "java", "cpp", "c", "rs", "rust":
		strategy = &CodeChunker{}
	default:
		strategy = &PlainTextChunker{}
	}
	return strategy.Chunk(content, sourceType)
}
