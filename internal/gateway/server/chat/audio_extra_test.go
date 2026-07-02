package chat

import (
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/llm/tts"

	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleAudioSpeech(t *testing.T) {
	h := &ChatHandler{DataDir: t.TempDir(),
		TTSEngine: new(atomic.Pointer[tts.ProviderBox]),
		STTEngine: new(atomic.Pointer[STTEngineBox])}

	// test without tts engine
	req := httptest.NewRequest("POST", "/api/v1/audio/speech", bytes.NewBufferString(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h.HandleAudioSpeech(w, req)
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 since no tts engine is set")
	}
}

func TestHandleAudioTranscriptions(t *testing.T) {
	h := &ChatHandler{DataDir: t.TempDir(),
		TTSEngine: new(atomic.Pointer[tts.ProviderBox]),
		STTEngine: new(atomic.Pointer[STTEngineBox])}

	// Create multipart body
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write([]byte("fake audio data"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	h.HandleAudioTranscriptions(w, req)
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 since no stt engine is set")
	}
}

func TestRespondJSON(t *testing.T) {
	w := httptest.NewRecorder()
	respondJSON(w, map[string]string{"foo": "bar"})
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200")
	}
}
