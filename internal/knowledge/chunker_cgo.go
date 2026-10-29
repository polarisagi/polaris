//go:build cgo

package knowledge

import (
	"context"
	"strings"
	"time"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
)

// CodeChunker 按 func 和 class 边界进行分块。
// [Task 15] 升级为使用 tree-sitter AST，支持 Go/Python/JS-TS，其他语言 fallback 字符串匹配。
// 注意：go-tree-sitter 依赖 CGO，由于 chunking 在离线/后台索引期执行，不在查询热路径，允许使用 CGO。
type CodeChunker struct{}

func (c *CodeChunker) Chunk(content, sourceType string) []string {
	var lang *sitter.Language
	var boundaryTypes map[string]bool

	switch sourceType {
	case "go":
		lang = golang.GetLanguage()
		boundaryTypes = map[string]bool{"function_declaration": true, "method_declaration": true, "type_declaration": true}
	case "py", "python":
		lang = python.GetLanguage()
		boundaryTypes = map[string]bool{"function_definition": true, "class_definition": true}
	case "js", "ts", "javascript", "typescript":
		lang = javascript.GetLanguage()
		boundaryTypes = map[string]bool{"function_declaration": true, "class_declaration": true, "method_definition": true, "arrow_function": true}
	}

	if lang != nil {
		return c.treeSitterChunk(content, lang, boundaryTypes)
	}

	return c.fallbackChunk(content)
}

func (c *CodeChunker) treeSitterChunk(content string, lang *sitter.Language, boundaryTypes map[string]bool) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(ctx, nil, []byte(content))
	if err != nil || tree == nil {
		return c.fallbackChunk(content)
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return c.fallbackChunk(content)
	}

	var finalChunks []string
	var current strings.Builder

	// 遍历顶级节点，如果是函数/类则单独成块
	for pos := 0; pos < int(root.NamedChildCount()); pos++ {
		child := root.NamedChild(pos)
		nodeType := child.Type()
		nodeText := content[child.StartByte():child.EndByte()]

		if boundaryTypes[nodeType] {
			// Flush current non-boundary nodes
			finalChunks = flushBuffer(&current, finalChunks)

			// Process boundary node
			p := strings.TrimSpace(nodeText)
			if p != "" {
				finalChunks = append(finalChunks, splitByLimit(p)...)
			}
		} else {
			current.WriteString(nodeText + "\n")
		}
	}

	finalChunks = flushBuffer(&current, finalChunks)

	// 如果因为某种原因切分失败（比如整个文件在一个大节点里），fallback 保护
	if len(finalChunks) == 0 && strings.TrimSpace(content) != "" {
		return c.fallbackChunk(content)
	}

	return finalChunks
}

func flushBuffer(current *strings.Builder, chunks []string) []string {
	if current.Len() > 0 {
		p := strings.TrimSpace(current.String())
		if p != "" {
			chunks = append(chunks, splitByLimit(p)...)
		}
		current.Reset()
	}
	return chunks
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
