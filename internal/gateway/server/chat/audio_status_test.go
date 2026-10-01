package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Content-Type 由 Provider 的 MIME 决定（HTTP sidecar 可能产出 audio/mpeg），而不是硬编码 audio/wav。
func TestSpeech_UsesProviderMIME(t *testing.T) {
	svc := newTestAudioService()
	svc.SetTTSEngine(fakeTTS{TTSAudio{Data: []byte("ID3mp3"), MIME: "audio/mpeg"}}, "http")

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

// notReadyErr 是满足 audioNotReady 的测试替身（真实实现为 audiorun.NotReadyError）。
type notReadyErr struct{ code, msg string }

func (e *notReadyErr) Error() string                   { return e.msg }
func (e *notReadyErr) AudioReason() (code, msg string) { return e.code, e.msg }

type errTTS struct{ err error }

func (f errTTS) Generate(context.Context, string) (TTSAudio, error) { return TTSAudio{}, f.err }

type errSTT struct{ err error }

func (f errSTT) Transcribe([]float32, int) (STTResult, error) { return STTResult{}, f.err }

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	return body
}

// TTS 引擎层返回"未安装"：503 JSON，error=tts_not_ready，附带状态机的 state，前端据此弹下载确认。
func TestSpeech_NotInstalledReturnsJSON503(t *testing.T) {
	svc := newTestAudioService()
	svc.TTSStatus.Replace(AudioAssetStatus{State: AudioStateNotInstalled, Detail: "未安装", InstallSizeBytes: 365 << 20})
	svc.SetTTSEngine(errTTS{&notReadyErr{"not_installed", "语音合成模型未安装"}}, "sherpa")

	w := httptest.NewRecorder()
	svc.HandleAudioSpeech(w, httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewBufferString(`{"input":"你好"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	body := decodeBody(t, w)
	if body["error"] != "tts_not_ready" || body["state"] != AudioStateNotInstalled || body["message"] == "" {
		t.Errorf("unexpected body: %v", body)
	}
}

// 不支持/内存不足/加载超时：error 即原因码，而不是被笼统归为 500。
func TestTranscriptions_NotReadyCodesPassThrough(t *testing.T) {
	for _, code := range []string{"insufficient_memory", "unsupported", "loading_timeout"} {
		svc := newTestAudioService()
		svc.SetSTTEngine(errSTT{&notReadyErr{code, "原因-" + code}})

		w := httptest.NewRecorder()
		svc.HandleAudioTranscriptions(w, wavRequest(t))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", code, w.Code)
		}
		body := decodeBody(t, w)
		if body["error"] != code || body["message"] != "原因-"+code {
			t.Errorf("%s: unexpected body: %v", code, body)
		}
	}
}

// 引擎层的普通错误仍是 500，不得被误当成"可恢复的未就绪"。
func TestTranscriptions_PlainErrorStays500(t *testing.T) {
	svc := newTestAudioService()
	svc.SetSTTEngine(errSTT{errors.New("ffi exploded")})
	w := httptest.NewRecorder()
	svc.HandleAudioTranscriptions(w, wavRequest(t))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

type fakeInstaller struct {
	started bool
	err     error
	kinds   []string
}

func (f *fakeInstaller) Install(kind string) (bool, error) {
	f.kinds = append(f.kinds, kind)
	return f.started, f.err
}

func TestInstall_StatusCodes(t *testing.T) {
	post := func(svc *AudioService, kind string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		svc.HandleAudioInstall(kind)(w, httptest.NewRequest(http.MethodPost, "/v1/audio/"+kind+"/install", nil))
		return w
	}

	// 未接线 → 503
	if w := post(newTestAudioService(), "stt"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("无安装器应 503，got %d", w.Code)
	}

	// 已开始 → 202 且带 started=true 与状态快照
	svc := newTestAudioService()
	fi := &fakeInstaller{started: true}
	svc.SetInstaller(fi)
	svc.TTSStatus.Replace(AudioAssetStatus{State: AudioStateDownloading, Detail: "下载"})
	w := post(svc, "tts")
	if w.Code != http.StatusAccepted {
		t.Fatalf("started 应 202，got %d", w.Code)
	}
	body := decodeBody(t, w)
	st, _ := body["status"].(map[string]any)
	if body["started"] != true || st["state"] != AudioStateDownloading {
		t.Errorf("unexpected body: %v", body)
	}
	if len(fi.kinds) != 1 || fi.kinds[0] != "tts" {
		t.Errorf("installer 应收到 kind=tts，got %v", fi.kinds)
	}

	// 无需安装 / 已在进行 → 200
	fi.started = false
	if w := post(svc, "stt"); w.Code != http.StatusOK {
		t.Errorf("无需安装应 200，got %d", w.Code)
	}

	// 不支持 → 422 + 原因码
	fi.err = &notReadyErr{"unsupported", "需要至少 4GB 内存 / 4 核"}
	w = post(svc, "tts")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("不支持应 422，got %d", w.Code)
	}
	if b := decodeBody(t, w); b["error"] != "unsupported" || !strings.Contains(b["message"].(string), "4GB") {
		t.Errorf("unexpected body: %v", b)
	}

	// 其它错误 → 500
	fi.err = errors.New("disk full")
	if w := post(svc, "stt"); w.Code != http.StatusInternalServerError {
		t.Errorf("普通错误应 500，got %d", w.Code)
	}
}

// 状态快照新字段（reason / install_size_bytes / progress / loaded）必须原样进入 JSON。
func TestAudioAssetStatus_JSONFields(t *testing.T) {
	tr := NewAudioStatusTracker("tts")
	tr.Replace(AudioAssetStatus{
		State: AudioStateDownloading, InstallSizeBytes: 365, Reason: "x",
		Progress: &AudioProgress{BytesDone: 10, BytesTotal: 100},
	})
	b, err := json.Marshal(tr.Get())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"install_size_bytes":365`, `"reason":"x"`, `"bytes_done":10`, `"bytes_total":100`, `"loaded":false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON 缺少 %s: %s", want, b)
		}
	}
}

// wavRequest 构造一个带合法 16kHz WAV 的转写请求（复用 audio_service_test.go 的 makeTestWAV）。
func wavRequest(t *testing.T) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("file", "record.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(makeTestWAV([]float32{0.1, -0.2, 0.3}, 16000)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}
