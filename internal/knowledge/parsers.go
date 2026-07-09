package knowledge

import (
	"bytes"
	"io"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// PDFChunker extracts text using pdfcpu and chunks it.
type PDFChunker struct{}

func (c *PDFChunker) Chunk(content, sourceType string) []string {
	// Interpret content as raw PDF bytes
	rs := bytes.NewReader([]byte(content))

	var allText strings.Builder
	err := api.ExtractContent(rs, nil, func(r io.Reader, page int) error {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r)
		allText.WriteString(buf.String())
		allText.WriteString("\n\n")
		return nil
	}, model.NewDefaultConfiguration())

	if err != nil {
		fallback := &PlainTextChunker{}
		return fallback.Chunk(content, sourceType)
	}

	extracted := strings.TrimSpace(allText.String())
	if extracted == "" {
		fallback := &PlainTextChunker{}
		return fallback.Chunk(content, sourceType)
	}

	fallback := &PlainTextChunker{}
	return fallback.Chunk(extracted, "txt")
}

// GoldmarkChunker uses goldmark AST to extract chunks by heading and block boundaries.
type GoldmarkChunker struct{}

func (c *GoldmarkChunker) Chunk(content, sourceType string) []string {
	md := goldmark.New()
	source := []byte(content)
	doc := md.Parser().Parse(text.NewReader(source))

	var chunks []string
	var current strings.Builder

	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch n.Kind() {
		case ast.KindHeading:
			if current.Len() > 0 {
				chunks = append(chunks, splitByLimit(strings.TrimSpace(current.String()))...)
				current.Reset()
			}
			// Include the heading text
			for i := 0; i < n.Lines().Len(); i++ {
				line := n.Lines().At(i)
				current.Write(line.Value(source))
			}
			current.WriteString("\n")
		case ast.KindParagraph, ast.KindCodeBlock, ast.KindFencedCodeBlock, ast.KindBlockquote:
			// Append blocks of text
			var buf bytes.Buffer
			for i := 0; i < n.Lines().Len(); i++ {
				line := n.Lines().At(i)
				buf.Write(line.Value(source))
			}
			current.WriteString(buf.String())
			current.WriteString("\n\n")
		}
		return ast.WalkContinue, nil
	})

	if err != nil {
		fallback := &PlainTextChunker{}
		return fallback.Chunk(content, sourceType)
	}

	if current.Len() > 0 {
		chunks = append(chunks, splitByLimit(strings.TrimSpace(current.String()))...)
	}

	if len(chunks) == 0 {
		fallback := &PlainTextChunker{}
		return fallback.Chunk(content, sourceType)
	}

	return chunks
}
