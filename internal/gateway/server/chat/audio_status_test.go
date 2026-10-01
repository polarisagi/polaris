package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type fakeTTS struct{ audio TTSAudio }

func (f fakeTTS) Generate(context.Context, string) (TTSAudio, error) { return f.audio, nil }

func newTestAudioService() *AudioService {
	return NewAudioService(new(atomic.Pointer[STTEngineBox]), new(atomic.Pointer[TTSProviderBox]), "", nil)
}

// STT 未就绪时返回 503 JSON（state/detail/message），而不是一行纯文本，前端才能提示原因。
func TestTranscriptions_NotReadyReturnsJSONStatus(t *testing.T) {
	svc := newTestAudioService()
	svc.STTStatus.Set(AudioStateDownloading, "下载 SenseVoice int8 166MB", "")

	w := httptest.NewRecorder()
	svc.HandleAudioTranscriptions(w, httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	if body["error"] != "stt_not_ready" || body["state"] != AudioStateDownloading || body["detail"] == "" || body["message"] == "" {
		t.Errorf("unexpected body: %v", body)
	}
}

// failed 状态下转写请求要唤醒一次立即重试；重复请求不堆积信号。
func TestTranscriptions_FailedKicksRetryOnce(t *testing.T) {
	svc := newTestAudioService()
	svc.STTStatus.Set(AudioStateFailed, "", "boom")

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		svc.HandleAudioTranscriptions(w, httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil))
	}
	select {
	case <-svc.STTRetrySignal():
	default:
		t.Fatal("expected a retry signal")
	}
	select {
	case <-svc.STTRetrySignal():
		t.Fatal("signals must not pile up beyond capacity 1")
	default:
	}
}

// Content-Type 由 Provider 的 MIME 决定（Edge=audio/mpeg），而不是硬编码 audio/wav。
func TestSpeech_UsesProviderMIME(t *testing.T) {
	svc := newTestAudioService()
	svc.SetTTSEngine(fakeTTS{TTSAudio{Data: []byte("ID3mp3"), MIME: "audio/mpeg"}}, "edge")

	w := httptest.NewRecorder()
	svc.HandleAudioSpeech(w, httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewBufferString(`{"input":"你好"}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}
	if w.Body.String() != "ID3mp3" {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestAudioStatusTracker_NilSafe(t *testing.T) {
	var tr *AudioStatusTracker
	tr.Set(AudioStateReady, "", "") // 不得 panic
	if got := tr.Get().State; got != AudioStatePending {
		t.Errorf("nil tracker Get = %q", got)
	}
}
