package knowledge

import (
	"strings"
	"testing"
)

func TestChunker_SplitByLimit(t *testing.T) {
	short := "This is a short sentence."
	chunks := splitByLimit(short)
	if len(chunks) != 1 || chunks[0] != short {
		t.Errorf("expected 1 chunk, got %v", chunks)
	}

	longWord := strings.Repeat("A", chunkMaxBytes+10)
	chunks = splitByLimit(longWord)
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	if len(chunks[0]) != chunkMaxBytes {
		t.Errorf("expected max length for chunk 0")
	}

	sentences := strings.Repeat("Sentence one. Sentence two. ", 100)
	chunks = splitByLimit(sentences)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks due to length")
	}
	for _, c := range chunks {
		if len(c) > chunkMaxBytes {
			t.Errorf("chunk exceeds max limit")
		}
	}
}

func TestPlainTextChunker(t *testing.T) {
	c := &PlainTextChunker{}
	content := "Para 1\n\nPara 2\n\n\nPara 3"
	chunks := c.Chunk(content, "txt")
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d", len(chunks))
	}
}

func TestMarkdownChunker(t *testing.T) {
	c := &MarkdownChunker{}
	content := "## Heading 1\nContent 1\n### Heading 2\nContent 2"
	chunks := c.Chunk(content, "md")
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0], "Heading 1") || !strings.Contains(chunks[1], "Heading 2") {
		t.Errorf("bad chunking boundaries")
	}
}

func TestCodeChunker(t *testing.T) {
	c := &CodeChunker{}
	content := "func Test() {\n  return 1;\n}\n\nclass MyClass {\n  do() {}\n}"
	chunks := c.Chunk(content, "go")
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0], "func Test") || !strings.Contains(chunks[1], "class MyClass") {
		t.Errorf("bad chunking boundaries")
	}
}

func TestDefaultChunker(t *testing.T) {
	c := &DefaultChunker{}
	if len(c.Chunk("## test", "md")) != 1 {
		t.Errorf("md failed")
	}
	if len(c.Chunk("func test", "go")) != 1 {
		t.Errorf("go failed")
	}
	if len(c.Chunk("test", "txt")) != 1 {
		t.Errorf("txt failed")
	}
}
