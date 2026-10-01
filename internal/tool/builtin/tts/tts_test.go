package tts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func okSynth(data []byte, mime string) Synthesizer {
	return func(context.Context, string) ([]byte, string, error) { return data, mime, nil }
}

func TestTTS_InvalidArgs(t *testing.T) {
	fn := MakeTTSFn(okSynth([]byte("x"), "audio/wav"))
	if _, err := fn(context.Background(), []byte("invalid")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := fn(context.Background(), []byte(`{"text":""}`)); err == nil {
		t.Fatal("空文本应报错")
	}
	long := `{"text":"` + strings.Repeat("字", maxTextRunes+1) + `"}`
	if _, err := fn(context.Background(), []byte(long)); err == nil {
		t.Fatal("超长文本应报错")
	}
}

// 成功路径：data URI 的 MIME 取自引擎实际产物，base64 可还原。
func TestTTS_Success(t *testing.T) {
	fn := MakeTTSFn(okSynth([]byte("RIFFwav"), "audio/wav"))
	out, err := fn(context.Background(), []byte(`{"text":"你好"}`))
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]string
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res["status"] != "success" || !strings.HasPrefix(res["audio_uri"], "data:audio/wav;base64,") {
		t.Errorf("unexpected result: %v", res)
	}
}

// 引擎失败必须如实返回错误，不得回出假音频并报 success（静默兜底，ADR-0106 的教训）。
func TestTTS_EngineFailureIsNotMasked(t *testing.T) {
	fn := MakeTTSFn(func(context.Context, string) ([]byte, string, error) {
		return nil, "", errors.New("语音合成模型未安装")
	})
	out, err := fn(context.Background(), []byte(`{"text":"test"}`))
	if err == nil {
		t.Fatalf("引擎失败时应返回错误，got output: %s", out)
	}
	if !strings.Contains(err.Error(), "未安装") {
		t.Errorf("错误应保留引擎给出的原因，got %v", err)
	}
}

func TestTTS_EmptyAudioAndNilSynthIsError(t *testing.T) {
	if _, err := MakeTTSFn(okSynth(nil, "audio/wav"))(context.Background(), []byte(`{"text":"a"}`)); err == nil {
		t.Error("空音频必须报错")
	}
	if _, err := MakeTTSFn(nil)(context.Background(), []byte(`{"text":"a"}`)); err == nil {
		t.Error("引擎未接线必须报错")
	}
}
