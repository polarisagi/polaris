package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type OTLPHTTPExporter struct {
	client   *http.Client
	endpoint string
}

func NewOTLPHTTPExporter(client *http.Client, endpoint string) *OTLPHTTPExporter {
	return &OTLPHTTPExporter{client: client, endpoint: endpoint}
}

func (e *OTLPHTTPExporter) ExportSpan(ctx context.Context, s *Span) error {
	// 简单的 JSON 导出
	b, err := json.Marshal(s)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "OTLPHTTPExporter: marshal failed", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.endpoint, bytes.NewReader(b))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "OTLPHTTPExporter: new request failed", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "OTLPHTTPExporter: send failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apperr.New(apperr.CodeInternal, "OTLPHTTPExporter: unexpected status")
	}
	return nil
}

func (e *OTLPHTTPExporter) Shutdown(ctx context.Context) error {
	return nil
}
