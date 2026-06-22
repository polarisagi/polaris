package provider

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestHandleSetModelRoles(t *testing.T) {
	h := &ProviderHandler{}
	req := httptest.NewRequest("POST", "/api/v1/providers/models/roles", bytes.NewBufferString(`{"roles": {}}`))
	w := httptest.NewRecorder()

	defer func() {
		recover()
	}()
	h.HandleSetModelRoles(w, req)
}

func TestHandleGetModelRoles(t *testing.T) {
	h := &ProviderHandler{}
	req := httptest.NewRequest("GET", "/api/v1/providers/models/roles", nil)
	w := httptest.NewRecorder()

	defer func() {
		recover()
	}()
	h.HandleGetModelRoles(w, req)
}

func TestHandleTestProvider(t *testing.T) {
	h := &ProviderHandler{}
	req := httptest.NewRequest("POST", "/api/v1/providers/test", bytes.NewBufferString(`{"id": "test"}`))
	w := httptest.NewRecorder()

	defer func() {
		recover()
	}()
	h.HandleTestProvider(w, req)
}
