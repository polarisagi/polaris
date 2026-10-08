//go:build nettest

package audioassets

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// 外部资产坐标契约：对清单里每个资产（5 个平台库 + 3 个模型）做 HEAD（跟随重定向），
// 断言 200 且 Content-Length 与清单字节数一致。
//
// 为什么存在：这些 URL 与字节数是硬编码的"假设"，曾经 6 个平台里 5 个下载 URL 是错的、
// int8 模型 URL 也是错的，而单测全绿——断言只校验了"话说了没有"。本测试把假设变成可执行校验；
// 字节数一致还能提前发现上游悄悄替换了同名资产（此时 sha256 也必然变了）。
// 依赖外网，不进默认 CI；改动音频资产清单必须手动 make audio-nettest。
func TestNet_ManifestAssetsReachableAndSizeMatches(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	for _, a := range All() {
		t.Run(a.File, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, a.URL(), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("HEAD %s: %v", a.URL(), err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HEAD %s = %d, want 200", a.URL(), resp.StatusCode)
			}
			if resp.ContentLength != a.Size {
				t.Errorf("Content-Length = %d, 清单 Size = %d（上游资产可能已被替换，需重算 sha256）", resp.ContentLength, a.Size)
			}
		})
	}
}
