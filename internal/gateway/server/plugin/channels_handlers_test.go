package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type stubChannels struct{ saved *types.PluginChannelState }

func (s *stubChannels) ListBindings(context.Context) ([]lifecycle.ChannelBinding, error) {
	return []lifecycle.ChannelBinding{{PluginID: "pl_t", Server: "tg"}}, nil
}

func (s *stubChannels) SetBindingState(_ context.Context, st types.PluginChannelState) error {
	if st.Server != "tg" {
		return apperr.New(apperr.CodeNotFound, "no channel")
	}
	s.saved = &st
	return nil
}

func TestPluginChannelHandlers(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	w := httptest.NewRecorder()
	h.HandleListPluginChannels(w, httptest.NewRequest("GET", "/v1/plugins/channels", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", w.Code)
	}
	ch := &stubChannels{}
	h.Channels = ch
	w = httptest.NewRecorder()
	h.HandleListPluginChannels(w, httptest.NewRequest("GET", "/v1/plugins/channels", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"server":"tg"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	put := func(body string) int {
		w := httptest.NewRecorder()
		h.HandleSetPluginChannel(w, httptest.NewRequest("PUT", "/v1/plugins/channels", strings.NewReader(body)))
		return w.Code
	}
	// 停用 channel 时审批转发一并关闭。
	if code := put(`{"plugin_id":"pl_t","server":"tg","enabled":false,"permission_relay":true}`); code != http.StatusOK || ch.saved.PermissionRelay {
		t.Fatalf("set: %d %+v", code, ch.saved)
	}
	if code := put(`{"plugin_id":"pl_t","server":"x","enabled":true}`); code != http.StatusNotFound {
		t.Fatalf("unknown channel: %d", code)
	}
	if code := put(`{}`); code != http.StatusBadRequest {
		t.Fatalf("missing fields: %d", code)
	}
}
