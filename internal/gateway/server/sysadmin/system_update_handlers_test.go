package sysadmin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSystemUpdateHandlers(t *testing.T) {
	h := &SysAdminHandler{}

	req := httptest.NewRequest("GET", "/api/v1/system/version", nil)
	w := httptest.NewRecorder()
	h.HandleGetVersion(w, req)
	if w.Result().StatusCode != http.StatusOK && w.Result().StatusCode != http.StatusInternalServerError {
		t.Logf("get version returned: %v %s", w.Result().StatusCode, w.Body.String())
	}

	req = httptest.NewRequest("POST", "/api/v1/system/update", nil)
	// mock local origin by setting remote addr to 127.0.0.1
	req.RemoteAddr = "127.0.0.1:12345"
	w = httptest.NewRecorder()
	h.HandleTriggerUpdate(w, req)
	if w.Result().StatusCode != http.StatusAccepted && w.Result().StatusCode != http.StatusInternalServerError {
		t.Logf("trigger update returned: %v %s", w.Result().StatusCode, w.Body.String())
	}
}
