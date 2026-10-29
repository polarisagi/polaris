package provider

import (
	"net/http"
	"os"
	"testing"

	"github.com/polarisagi/polaris/internal/llm/adapter"
)

func TestMain(m *testing.M) {
	adapter.SetDefaultHTTPClient(&http.Client{})
	os.Exit(m.Run())
}
