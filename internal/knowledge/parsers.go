package knowledge

import (
	"bytes"
	"io"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
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
