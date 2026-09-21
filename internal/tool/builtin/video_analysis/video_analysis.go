package video_analysis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ExecuteVideoAnalysis 执行视频分析。元数据由 builtin/video_analysis/tool.yaml + schema.json 定义。

// maxRemoteVideoBytes 远程视频下载上限：ffmpeg 抽帧前先经 SafeDialer 落到临时目录。
const maxRemoteVideoBytes = 200 << 20

// MakeExecuteVideoAnalysisFn 执行视频分析。
//
// 安全边界（GR-5.2-001）：
//   - 本地路径必须通过宿主 allowedPaths 白名单与 CheckForbiddenPath 黑名单，此前直接放行
//     入参所在目录，任意主机路径均可喂给 ffmpeg；
//   - 远程 URL 经 SafeDialer（SSRF/DNS rebinding 防护）下载到临时目录，ffmpeg 永不联网，
//     此前由 ffmpeg 直连，完全绕过出站防护；
//   - 抽帧失败返回错误，不再伪造 mock 帧冒充成功结果。
func MakeExecuteVideoAnalysisFn(allowedPaths []string, dialer protocol.SafeDialer, sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, args []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var req struct {
			VideoURI    string `json:"video_uri"`
			IntervalSec int    `json:"interval_sec"`
			MaxFrames   int    `json:"max_frames"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid args", err)
		}
		if req.VideoURI == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "video_analysis: video_uri is required")
		}
		if req.IntervalSec <= 0 {
			req.IntervalSec = 5
		}
		if req.MaxFrames <= 0 {
			req.MaxFrames = 20
		}

		tmpDir, err := os.MkdirTemp("", "polaris_video_")
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "video_analysis: create temp dir", err)
		}
		defer os.RemoveAll(tmpDir)

		input, inputDir, err := resolveVideoInput(ctx, req.VideoURI, tmpDir, paths, dialer)
		if err != nil {
			return nil, err
		}
		allowed := []string{tmpDir}
		if inputDir != tmpDir {
			allowed = append(allowed, inputDir)
		}

		framesDir := filepath.Join(tmpDir, "frames")
		if err := os.Mkdir(framesDir, 0o700); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "video_analysis: create frames dir", err)
		}
		fpsArg := fmt.Sprintf("fps=1/%d", req.IntervalSec)
		ffmpegArgs := []string{"-i", input, "-vf", fpsArg, "-frames:v", fmt.Sprint(req.MaxFrames), filepath.Join(framesDir, "%04d.jpg")}
		if _, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "ffmpeg", ffmpegArgs, tmpDir, allowed, false, 60000, sandboxEnabled, bwrapPath); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "video_analysis: ffmpeg frame extraction failed", err)
		}
		entries, _ := os.ReadDir(framesDir)
		frames := processKeyFrames(framesDir, entries)
		if len(frames) == 0 {
			return nil, apperr.New(apperr.CodeInvalidInput, "video_analysis: no frames extracted (unsupported or empty video)")
		}
		if len(frames) > req.MaxFrames {
			frames = frames[:req.MaxFrames]
		}

		result := map[string]any{
			"status":  "extracted",
			"frames":  frames,
			"message": fmt.Sprintf("Extracted %d keyframes from %s at %ds interval", len(frames), req.VideoURI, req.IntervalSec),
		}
		return json.Marshal(result)
	}
}

// resolveVideoInput 返回 ffmpeg 输入路径及需放行的目录。
func resolveVideoInput(ctx context.Context, uri, tmpDir string, allowedPaths []string, dialer protocol.SafeDialer) (string, string, error) {
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		dst := filepath.Join(tmpDir, "input.video")
		if err := downloadVideo(ctx, uri, dst, dialer); err != nil {
			return "", "", err
		}
		return dst, tmpDir, nil
	}
	if strings.Contains(uri, "://") {
		return "", "", apperr.New(apperr.CodeInvalidInput, "video_analysis: only local paths and http(s) URLs are supported")
	}
	abs, err := filepath.Abs(uri)
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeInvalidInput, "video_analysis: invalid path", err)
	}
	if err := guard.CheckAllowedPath(abs, allowedPaths); err != nil {
		return "", "", apperr.Wrap(apperr.CodeForbidden, "video_analysis", err)
	}
	if err := guard.CheckForbiddenPath(abs); err != nil {
		return "", "", apperr.Wrap(apperr.CodeForbidden, "video_analysis", err)
	}
	return abs, filepath.Dir(abs), nil
}

func downloadVideo(ctx context.Context, url, dst string, dialer protocol.SafeDialer) error {
	if dialer == nil {
		return apperr.New(apperr.CodeInternal, "video_analysis: SafeDialer is required for remote video (XR-06)")
	}
	client := &http.Client{
		Transport: network.WrapCapability(&http.Transport{DialContext: dialer.DialContext}, network.CapNetworkRead),
		Timeout:   120 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "video_analysis: bad url", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeNetworkUnavailable, "video_analysis: download failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("video_analysis: download status %d", resp.StatusCode))
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "video_analysis: create download file", err)
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxRemoteVideoBytes+1))
	if err != nil {
		return apperr.Wrap(apperr.CodeNetworkUnavailable, "video_analysis: download read", err)
	}
	if n > maxRemoteVideoBytes {
		return apperr.New(apperr.CodeResourceExhausted, "video_analysis: remote video exceeds 200MB limit")
	}
	return nil
}

func processKeyFrames(tmpDir string, entries []os.DirEntry) []string {
	var frames []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jpg") {
			data, err := os.ReadFile(filepath.Join(tmpDir, entry.Name()))
			if err == nil {
				frames = append(frames, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(data))
			}
		}
	}
	return frames
}
