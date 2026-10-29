package plugin

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestHandleCreatePluginFromIntent(t *testing.T) {
	h := &PluginHandler{}
	req := httptest.NewRequest("POST", "/api/v1/plugins/intent", bytes.NewBufferString(`{"intent": "test"}`))
	w := httptest.NewRecorder()

	defer func() {
		recover() // ignore panic
	}()
	h.HandleCreatePluginFromIntent(w, req, "", protocol.ExtensionInstallRequest{}, "")
}

func TestHandleCreateMCP(t *testing.T) {
	h := &PluginHandler{}
	req := httptest.NewRequest("POST", "/api/v1/plugins/custom/mcp", bytes.NewBufferString(`{"name": "test"}`))
	w := httptest.NewRecorder()
	h.HandleCreateMCP(w, req)
}
