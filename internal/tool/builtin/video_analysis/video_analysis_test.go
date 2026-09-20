package video_analysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// GR-5.2-001：路径白名单/黑名单、远程下载必须经 SafeDialer、失败不得伪造 mock 帧。
func TestExecuteVideoAnalysis_Guards(t *testing.T) {
	allowed := t.TempDir()
	fn := MakeExecuteVideoAnalysisFn([]string{allowed}, nil, false, "")
	ctx := context.Background()

	cases := map[string]string{
		"invalid json":       "invalid",
		"missing uri":        `{}`,
		"unsupported scheme": `{"video_uri":"file://test.mp4"}`,
		"outside allowed":    `{"video_uri":"/etc/passwd"}`,
		"remote no dialer":   `{"video_uri":"https://example.com/v.mp4"}`,
	}
	for name, in := range cases {
		if _, err := fn(ctx, []byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	// 白名单内但不是合法视频：ffmpeg 失败（或不存在）时必须返回错误，而不是 mock 帧。
	bogus := filepath.Join(allowed, "not_a_video.mp4")
	if err := os.WriteFile(bogus, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := fn(ctx, []byte(`{"video_uri":"`+bogus+`"}`)); err == nil {
		t.Fatalf("expected extraction error for invalid video, got %s", out)
	}
}
