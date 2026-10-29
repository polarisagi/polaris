package cronadmin

import (
	"sync"

	"net/http"
	"testing"
)

func TestFetchRemoteTemplates(t *testing.T) {
	ca := &CronAdmin{TemplateCacheMap: &sync.Map{}, HTTPClient: &http.Client{Timeout: 1}}
	_ = ca.fetchRemoteTemplates(automationSource{Type: "http", URL: "http://127.0.0.1:0/test"})
}
