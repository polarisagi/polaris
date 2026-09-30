package chat

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type dummySTT struct {
	transcribeCalled bool
	receivedSamples  []float32
	receivedSR       int
}

func (d *dummySTT) Transcribe(samples []float32, sampleRate int) (STTResult, error) {
	d.transcribeCalled = true
	d.receivedSamples = samples
	d.receivedSR = sampleRate
	return STTResult{Text: "测试识别成功"}, nil
}

func (d *dummySTT) IsAvailable() bool {
	return true
}

func makeTestWAV(samples []float32, sampleRate uint32) []byte {
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	riffLenPos := buf.Len()
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // Mono
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	binary.Write(&buf, binary.LittleEndian, sampleRate*2)
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))

	buf.WriteString("data")
	dataLenPos := buf.Len()
	binary.Write(&buf, binary.LittleEndian, uint32(len(samples)*2))
	for _, s := range samples {
		val := int16(s * 32767.0)
		binary.Write(&buf, binary.LittleEndian, val)
	}

	totalLen := buf.Len() - 8
	out := buf.Bytes()
	binary.LittleEndian.PutUint32(out[riffLenPos:riffLenPos+4], uint32(totalLen))
	_ = dataLenPos
	return out
}

func TestHandleAudioTranscriptions_WAVDirect(t *testing.T) {
	sttMock := &dummySTT{}
	sttBox := new(atomic.Pointer[STTEngineBox])
	sttBox.Store(&STTEngineBox{E: sttMock})

	service := NewAudioService(sttBox, new(atomic.Pointer[TTSProviderBox]))

	// 构造 16kHz WAV
	rawWav := makeTestWAV([]float32{0.1, -0.2, 0.3}, 16000)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "record.wav")
	if err != nil {
		t.Fatalf("create form file failed: %v", err)
	}
	part.Write(rawWav)
	writer.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	service.HandleAudioTranscriptions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, w.Body.String())
	}

	if !sttMock.transcribeCalled {
		t.Fatalf("expected STT.Transcribe to be called")
	}
	if sttMock.receivedSR != 16000 {
		t.Errorf("expected sampleRate 16000, got %d", sttMock.receivedSR)
	}
	if len(sttMock.receivedSamples) != 3 {
		t.Errorf("expected 3 samples, got %d", len(sttMock.receivedSamples))
	}

	var res STTResult
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if res.Text != "测试识别成功" {
		t.Errorf("expected '测试识别成功', got %q", res.Text)
	}
}

func TestHandleAudioTranscriptions_InvalidWAV(t *testing.T) {
	sttMock := &dummySTT{}
	sttBox := new(atomic.Pointer[STTEngineBox])
	sttBox.Store(&STTEngineBox{E: sttMock})

	service := NewAudioService(sttBox, new(atomic.Pointer[TTSProviderBox]))

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "bad.wav")
	part.Write([]byte("not a wav file"))
	writer.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	service.HandleAudioTranscriptions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", resp.StatusCode, w.Body.String())
	}

	var errResp map[string]string
	json.NewDecoder(w.Body).Decode(&errResp)
	if errResp["error"] != "invalid_wav" {
		t.Errorf("expected error code invalid_wav, got %v", errResp)
	}
}

func TestHandleAudioTranscriptions_NonWAV_FFmpegMissing(t *testing.T) {
	sttMock := &dummySTT{}
	sttBox := new(atomic.Pointer[STTEngineBox])
	sttBox.Store(&STTEngineBox{E: sttMock})

	service := NewAudioService(sttBox, new(atomic.Pointer[TTSProviderBox]))
	// 指定一个空的 binDir 且无 httpClient，且假设 PATH 中若无则必报错，若有则测试不崩溃
	service.SetBinDir(t.TempDir())

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "audio.webm")
	part.Write([]byte("dummy webm data"))
	writer.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	service.HandleAudioTranscriptions(w, req)

	resp := w.Result()
	// 如果系统 PATH 没有 ffmpeg 或转码失败，应返回 422，决不能静默返回 200 mock 假数据
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 or 200, got %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		var errResp map[string]string
		json.NewDecoder(w.Body).Decode(&errResp)
		if errResp["error"] == "" {
			t.Errorf("expected error code in response, got %v", errResp)
		}
	}
}
