//go:build nettest

package stt

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/config"

	"github.com/pelletier/go-toml/v2"
)

// 外部资产坐标契约：对全部平台的 sherpa 库 URL 与 defaults.toml 中的 3 个模型 URL
// 做 HEAD（跟随重定向）并断言 200。
//
// 为什么存在：这些 URL 是硬编码的"假设"，曾经 6 个平台里 5 个下载 URL 是错的、int8 模型
// URL 也是错的，而单测全绿——断言只校验了"话说了没有"。本测试把假设变成可执行校验。
// 依赖外网，不进默认 CI；改动音频资产坐标必须手动 make audio-nettest。
func TestNet_AssetURLsReachable(t *testing.T) {
	var urls []string
	for _, plat := range SherpaLibPlatforms() {
		goos, goarch, _ := cutSlash(plat)
		u, err := SherpaLibURLFor(goos, goarch, SherpaABIVersion)
		if err != nil {
			t.Fatalf("%s: %v", plat, err)
		}
		urls = append(urls, u)
	}

	var cfg config.Config
	data, err := configs.FS.ReadFile("defaults.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	urls = append(urls,
		cfg.Inference.STT.SenseVoiceModelURL,
		cfg.Inference.STT.SenseVoiceModelURLStd,
		cfg.Inference.STT.PunctModelURL,
	)

	client := &http.Client{Timeout: 60 * time.Second}
	for _, u := range urls {
		u := u
		t.Run(u[len(u)-40:], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("HEAD %s: %v", u, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("HEAD %s = %d, want 200", u, resp.StatusCode)
			}
		})
	}
}

func cutSlash(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
