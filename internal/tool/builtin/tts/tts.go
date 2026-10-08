// Package tts 是 `tts` 内置工具：把文本合成为语音，返回 data URI。
//
// 它不自带任何引擎，只调用调用方注入的本地 TTS Provider（MeloTTS / Matcha，经 AudioService，ADR-0110）。
// 此前的 tts_edge 靠外部 `edge-tts` CLI 访问微软在线端点，ADR-0107 随 Edge 整条删除。
package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Synthesizer 是本工具对 TTS 引擎的消费端接口（HE-3：接口在调用方定义）。
// 引擎未安装/不支持/内存不足时必须返回错误，不得返回空音频。
type Synthesizer func(ctx context.Context, text string) (data []byte, mime string, err error)

// maxTextRunes 限制单次合成的文本长度：服务端 TTS 在 RTF≈0.5 时 1000 个汉字约 4 分钟音频、
// 2 分钟推理，更长的文本会让一次工具调用占住推理引擎（同一引擎串行服务所有朗读请求）。
const maxTextRunes = 1000

// MakeTTSFn 返回文本转语音工具。元数据由 builtin/tts/tool.yaml + schema.json 定义。
// synth 为 nil（装配遗漏）时每次调用都如实报错，而不是回出假音频。
func MakeTTSFn(synth Synthesizer) sandbox.InProcessFn {
	return func(ctx context.Context, args []byte) ([]byte, error) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid args", err)
		}
		if req.Text == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "tts: text is required")
		}
		if n := utf8.RuneCountInString(req.Text); n > maxTextRunes {
			return nil, apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("tts: text too long (%d characters, max %d)", n, maxTextRunes))
		}
		if synth == nil {
			return nil, apperr.New(apperr.CodeUnimplemented, "tts: 本地 TTS 引擎未接线")
		}

		data, mime, err := synth(ctx, req.Text)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tts: synthesis failed", err)
		}
		if len(data) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "tts: engine produced empty audio")
		}
		if mime == "" {
			mime = "audio/wav"
		}
		return json.Marshal(map[string]string{
			"audio_uri": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
			"status":    "success",
			"message":   "Text converted to speech successfully",
		})
	}
}
