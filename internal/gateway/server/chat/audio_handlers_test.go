package chat

import (
	"sync/atomic"

	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAudioHandlers_EngineNotInitialized(t *testing.T) {
	h := &ChatHandler{DataDir: t.TempDir(),
		TTSEngine: new(atomic.Pointer[TTSProviderBox]),
		STTEngine: new(atomic.Pointer[STTEngineBox])}

	// Test SetTTSEngine with nil
	h.SetTTSEngine(nil)
	h.SetSTTEngine(nil)

	// Test handleAudioSpeech
	body := `{"input": "hello"}`
	req := httptest.NewRequest("POST", "/api/v1/audio/speech", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.HandleAudioSpeech(w, req)
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Result().StatusCode)
	}

	// Test handleAudioTranscriptions
	req2 := httptest.NewRequest("POST", "/api/v1/audio/transcriptions", nil)
	w2 := httptest.NewRecorder()

	h.HandleAudioTranscriptions(w2, req2)
	if w2.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w2.Result().StatusCode)
	}
}
