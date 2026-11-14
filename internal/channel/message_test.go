package channel

import (
	"net/http"
	"net/url"
	"testing"
)

func TestExtractMessage_Coverage(t *testing.T) {
	req := &http.Request{Form: url.Values{}}

	ExtractMessage("webhook", []byte(`{"content":"test"}`), req)
	ExtractMessage("unknown", []byte("{}"), req)
	ExtractMessage("telegram", []byte("{}"), req)
}
