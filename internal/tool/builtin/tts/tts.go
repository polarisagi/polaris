package tts_edge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// MakeExecuteEdgeTTSFn 返回文本转语音工具。元数据由 builtin/tts_edge/tool.yaml + schema.json 定义。
func MakeExecuteEdgeTTSFn(sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, args []byte) ([]byte, error) {
		var req struct {
			Text  string `json:"text"`
			Voice string `json:"voice"`
			Rate  string `json:"rate"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid args", err)
		}
		if req.Voice == "" {
			req.Voice = "en-US-AriaNeural"
		}
		if req.Rate == "" {
			req.Rate = "+0%"
		}

		// 调用真实的 edge-tts CLI 工具。失败必须如实返回错误：此前这里回出一段写死的假 MP3
		// 并报 success，调用方（Agent）无从得知语音根本没合成出来（静默兜底）。
		tmpFile, err := os.CreateTemp("", "polaris_tts_*.mp3")
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tts_edge: create temp file failed", err)
		}
		tmpPath := tmpFile.Name()
		tmpFile.Close()
		defer os.Remove(tmpPath)

		// 调用 sandbox 执行 edge-tts，允许网络（edge-tts 需要访问微软接口）。
		// 路径白名单收紧到 tmpPath 所在目录（临时目录），不放行整个文件系统——
		// edge-tts 只需要写这一个 mp3 文件，没有理由拿到全盘读写权限。
		edgeArgs := []string{"--text", req.Text, "--voice", req.Voice, "--rate", req.Rate, "--write-media", tmpPath}
		tmpDir := filepath.Dir(tmpPath)

		// netAllow = true (edge-tts 需要网络)
		if _, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "edge-tts", edgeArgs, tmpDir, []string{tmpDir}, true, 30000, sandboxEnabled, bwrapPath); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tts_edge: edge-tts execution failed (is the edge-tts CLI installed?)", err)
		}
		data, err := os.ReadFile(tmpPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tts_edge: read synthesized audio failed", err)
		}
		if len(data) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "tts_edge: edge-tts produced an empty audio file")
		}
		audioURI := "data:audio/mp3;base64," + base64.StdEncoding.EncodeToString(data)

		result := map[string]string{
			"audio_uri": audioURI,
			"status":    "success",
			"message":   "Text converted to speech successfully",
		}
		return json.Marshal(result)
	}
}
