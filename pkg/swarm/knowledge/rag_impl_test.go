package knowledge

import (
	"strings"
	"testing"
)

func TestChunkDocument_PreservesParagraphBoundary(t *testing.T) {
	pipeline := &DefaultIngestionPipeline{}
	content := "Para1 line1\nPara1 line2\n\nPara2 line1"
	chunks := pipeline.chunkDocument(content, "doc1", 0, DocumentRef{})

	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	// It should preserve paragraphs.
	hasPara1 := false
	hasPara2 := false
	for _, c := range chunks {
		if c.ChunkType == "parent" {
			if strings.Contains(c.Content, "Para1 line1\nPara1 line2") && strings.Contains(c.Content, "Para2 line1") {
				// Combined due to maxRunes
				hasPara1 = true
				hasPara2 = true
			}
		}
	}
	if !hasPara1 || !hasPara2 {
		t.Errorf("expected parent chunk to contain the paragraphs. chunks: %+v", chunks)
	}
}

func TestChunkDocument_ChineseSentenceBoundary(t *testing.T) {
	pipeline := &DefaultIngestionPipeline{}
	// string length is 10 runes per sentence, total 20 runes. maxRunes is 250, so they fit in 1 parent, 1 leaf.
	// Wait, maxRunes is 250, so it will not split.
	// To test Chinese sentence boundary, let's create a text longer than 250 runes with a sentence boundary near 250.
	content := strings.Repeat("字", 240) + "！" + strings.Repeat("字", 20)
	chunks := pipeline.chunkDocument(content, "doc1", 0, DocumentRef{})

	// Should be 1 parent chunk, and 2 leaf chunks.
	var leaves []Chunk
	for _, c := range chunks {
		if c.ChunkType == "leaf" {
			leaves = append(leaves, c)
		}
	}
	if len(leaves) != 2 {
		t.Fatalf("expected 2 leaf chunks, got %d", len(leaves))
	}
	if !strings.HasSuffix(leaves[0].Content, "！") {
		t.Errorf("expected first leaf to end with '！', got %q", leaves[0].Content)
	}
	if len([]rune(leaves[0].Content)) != 241 {
		t.Errorf("expected first leaf to have length 241, got %d", len([]rune(leaves[0].Content)))
	}
}

func TestChunkDocument_FallbackOnLongParagraph(t *testing.T) {
	pipeline := &DefaultIngestionPipeline{}
	// A very long string without any sentence boundaries
	content := strings.Repeat("a", 2000)
	chunks := pipeline.chunkDocument(content, "doc1", 0, DocumentRef{})

	var parents []Chunk
	var leaves []Chunk
	for _, c := range chunks {
		if c.ChunkType == "parent" {
			parents = append(parents, c)
		} else if c.ChunkType == "leaf" {
			leaves = append(leaves, c)
		}
	}

	if len(parents) != 2 {
		t.Fatalf("expected 2 parent chunks, got %d", len(parents))
	}
	if len(leaves) != 8 {
		t.Fatalf("expected 8 leaf chunks, got %d", len(leaves))
	}
}

func TestChunkDocument_EmptyContent(t *testing.T) {
	pipeline := &DefaultIngestionPipeline{}
	chunks := pipeline.chunkDocument("", "doc1", 0, DocumentRef{})
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks, got %d", len(chunks))
	}
}
