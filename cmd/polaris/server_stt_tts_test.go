package main

import (
	"context"
	"errors"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/llm/audiorun"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
)

// audiorun 与 chat 各自持有一份状态字符串常量（llm 层不得 import gateway 层），适配器直接透传——
// 两边任何一侧改字面都会让前端读到未知状态，必须由测试守住。
func TestAudioStateConstants_InSyncWithChat(t *testing.T) {
	pairs := map[string]string{
		audiorun.StateNotInstalled: chat.AudioStateNotInstalled,
		audiorun.StateUnsupported:  chat.AudioStateUnsupported,
		audiorun.StateDownloading:  chat.AudioStateDownloading,
		audiorun.StateLoading:      chat.AudioStateLoading,
		audiorun.StateReady:        chat.AudioStateReady,
		audiorun.StateFailed:       chat.AudioStateFailed,
	}
	for a, c := range pairs {
		if a != c {
			t.Errorf("状态常量漂移: audiorun=%q chat=%q", a, c)
		}
	}
}

func TestToChatStatus_MapsAllFields(t *testing.T) {
	got := toChatStatus(audiorun.Status{
		State: audiorun.StateDownloading, Detail: "d", Error: "e", Reason: "r",
		InstallSizeBytes: 7, Loaded: true, BytesDone: 3, BytesTotal: 9,
	})
	if got.State != "downloading" || got.Detail != "d" || got.Error != "e" || got.Reason != "r" ||
		got.InstallSizeBytes != 7 || !got.Loaded || got.Progress == nil ||
		got.Progress.BytesDone != 3 || got.Progress.BytesTotal != 9 {
		t.Errorf("字段映射不完整: %+v", got)
	}
	if toChatStatus(audiorun.Status{State: audiorun.StateReady}).Progress != nil {
		t.Error("无进度时不应带 progress 字段")
	}
}

type stubSTT struct{ err error }

func (s stubSTT) Transcribe([]float32, int) (stt.Result, error) { return stt.Result{Text: "x"}, s.err }

type stubTTS struct{ err error }

func (s stubTTS) Generate(context.Context, string) (tts.Audio, error) {
	return tts.Audio{Data: []byte("w"), MIME: "audio/wav"}, s.err
}
func (s stubTTS) Close() error { return nil }

// NotReadyError 必须原样穿过适配器：被包成 500 会让前端丢失"需要下载/硬件不支持"这类可处理的原因。
func TestAdapters_PassNotReadyThrough(t *testing.T) {
	nr := &audiorun.NotReadyError{Code: audiorun.CodeNotInstalled, Message: "未安装"}

	_, err := (&sttAdapter{inner: stubSTT{err: nr}}).Transcribe(nil, 16000)
	if got, ok := audiorun.AsNotReady(err); !ok || got.Code != audiorun.CodeNotInstalled {
		t.Errorf("stt 适配器应透传 NotReadyError，got %v", err)
	}
	_, err = (&ttsAdapter{inner: stubTTS{err: nr}}).Generate(context.Background(), "x")
	if got, ok := audiorun.AsNotReady(err); !ok || got.Code != audiorun.CodeNotInstalled {
		t.Errorf("tts 适配器应透传 NotReadyError，got %v", err)
	}

	// 普通错误仍包装（保留 500 语义）。
	_, err = (&sttAdapter{inner: stubSTT{err: errors.New("ffi")}}).Transcribe(nil, 16000)
	if err == nil {
		t.Fatal("普通错误应上报")
	}
	if _, ok := audiorun.AsNotReady(err); ok {
		t.Error("普通错误不得被当成 NotReady")
	}
}

func TestTTSBridge_UnboundReportsErrorThenDelegates(t *testing.T) {
	b := &ttsBridge{}
	if _, _, err := b.Synthesize(context.Background(), "x"); err == nil {
		t.Fatal("未 Bind 必须报错，不得回出假音频")
	}
	b.Bind(func(context.Context, string) (chat.TTSAudio, error) {
		return chat.TTSAudio{Data: []byte("RIFF"), MIME: "audio/wav"}, nil
	})
	data, mime, err := b.Synthesize(context.Background(), "x")
	if err != nil || string(data) != "RIFF" || mime != "audio/wav" {
		t.Errorf("Bind 后应委托: %q %q %v", data, mime, err)
	}
	b.Bind(func(context.Context, string) (chat.TTSAudio, error) { return chat.TTSAudio{}, errors.New("boom") })
	if _, _, err := b.Synthesize(context.Background(), "x"); err == nil {
		t.Error("下游错误必须上报")
	}
}

func TestTrimLangTag(t *testing.T) {
	for in, want := range map[string]string{"<|yue|>": "yue", "<|zh|>": "zh", "en": "en", "": "", " <|ja|> ": "ja"} {
		if got := trimLangTag(in); got != want {
			t.Errorf("trimLangTag(%q)=%q want %q", in, got, want)
		}
	}
}
