package channel

import (
	"net/http"
	"net/url"
	"testing"
)

func TestExtractMessage_Coverage(t *testing.T) {
	req := &http.Request{Form: url.Values{}}

	ExtractMessage("telegram", []byte("{}"), req)
	ExtractMessage("discord", []byte("{}"), req)
	ExtractMessage("slack", []byte("{}"), req)
	ExtractMessage("feishu", []byte("{}"), req)
	ExtractMessage("line", []byte("{}"), req)
	ExtractMessage("qqbot", []byte("{}"), req)
	ExtractMessage("whatsapp", []byte("{}"), req)
	ExtractMessage("sms", []byte("{}"), req)
	ExtractMessage("teams", []byte("{}"), req)
	ExtractMessage("webhook", []byte("{}"), req)
	ExtractMessage("unknown", []byte("{}"), req)
}
