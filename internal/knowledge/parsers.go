package knowledge

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// PDFChunker extracts text using pdfcpu and chunks it.
type PDFChunker struct{}

func (c *PDFChunker) Chunk(content, sourceType string) []string {
	// Interpret content as raw PDF bytes
	rs := bytes.NewReader([]byte(content))

	var allText strings.Builder
	err := api.ExtractContent(rs, nil, func(r io.Reader, page int) error {
		buf := new(bytes.Buffer)
		// ReadFrom 错误必须上抛：吞掉的话，I/O 中断只表现为该页文本被静默截断，
		// 摄取流程照常成功，最终得到一份"看起来完整"的残缺知识库。
		// 返回 error 会让 ExtractContent 整体失败，外层随即回退 PlainTextChunker。
		if _, rfErr := buf.ReadFrom(r); rfErr != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("pdf: read page %d content", page), rfErr)
		}
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
