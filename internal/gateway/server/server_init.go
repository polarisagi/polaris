package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
	webui "github.com/polarisagi/polaris/web"
)

// seedBuiltinConfig 将 embedded yaml 配置作为种子数据写入数据库（INSERT OR IGNORE）。
//
//nolint:nestif
func (s *Server) SeedBuiltinConfig(marketplacesData, registryData []byte) {
	if len(marketplacesData) > 0 {
		var mps []protocol.Marketplace
		if err := yaml.Unmarshal(marketplacesData, &mps); err == nil {
			now := time.Now().UTC().Format(time.RFC3339)
			for _, mp := range mps {
				mp.CreatedAt = now
				if err := s.extRepo.SeedMarketplace(context.Background(), mp); err != nil {
					slog.Error("polaris-server: SeedMarketplace failed", "err", err, "id", mp.ID)
				}
			}
		} else {
			slog.Warn("polaris-server: configs/extensions/marketplaces.yaml parse failed", "err", err)
		}
	} else {
		slog.Warn("polaris-server: configs/extensions/marketplaces.yaml load failed (empty)")
	}

	if len(registryData) > 0 {
		var entries []protocol.RegistryEntry
		if err := yaml.Unmarshal(registryData, &entries); err == nil {
			for _, e := range entries {
				payload, _ := json.Marshal(e)
				_ = s.extRepo.SeedCatalogEntry(context.Background(), types.ExtCatalogRow{
					ID:            e.ID,
					MarketplaceID: "builtin",
					Type:          e.Type,
					Name:          e.Name,
					Description:   e.Description,
					Publisher:     e.Publisher,
					TrustTier:     e.TrustTier,
					URL:           e.URL,
					Payload:       string(payload),
				})
			}
		} else {
			slog.Warn("polaris-server: configs/extensions/registry.yaml parse failed", "err", err)
		}
	} else {
		slog.Warn("polaris-server: configs/extensions/registry.yaml load failed (empty)")
	}
}

// InitSTTEngine 按 FeatureGate 门控初始化 STT 引擎。
// 必须在 NewServer 之后、Start 之前调用（或与 Start 并发，mock 引擎已就绪）。
// 流程：
//  1. 立即注入 mock 引擎（保证 /v1/audio/transcriptions 不返回 503）
//  2. 若门控禁用，仅打 Info 日志后返回
//  3. 否则在后台 goroutine：EnsureAssets → LoadLibrary → NewEngine → 替换为真实引擎
func (s *Server) InitSTTEngine(ctx context.Context, dataDir string, gate *probe.FeatureGate, httpClient *http.Client, sttConfig config.STTConfig) {
	sttDir := filepath.Join(dataDir, "models", "sensevoice")

	// 立即设置 mock 引擎，保证接口可用
	// if mockEngine...

	// 门控检查：FeatureLocalSTT 是最低档（int8），未开启则无法运行 STT
	if gate != nil && gate.State(probe.FeatureLocalSTT) == probe.FeatureDisabled {
		slog.Info("stt: FeatureLocalSTT disabled by FeatureGate (need ≥512MB free), using mock engine")
		return
	}

	// 按 FeatureHQSTT 自动选择模型档位：
	//   HQ 门控开启（≥1GB free）→ float32 SenseVoice（精度优先）
	//   HQ 门控未开启              → int8 SenseVoice（速度/体积优先）
	useHQ := gate != nil && gate.State(probe.FeatureHQSTT) != probe.FeatureDisabled
	modelURL := sttConfig.SenseVoiceModelURL // 默认 float32（HQ）
	if !useHQ {
		if sttConfig.SenseVoiceModelURLStd != "" {
			modelURL = sttConfig.SenseVoiceModelURLStd // int8 标准档
		}
		// SenseVoiceModelURLStd 为空（旧配置）则回退到 SenseVoiceModelURL
	}

	// 异步下载 + 重载：不阻塞启动路径
	concurrent.SafeGo(ctx, "gateway.server.stt_asset_download", func(ctx context.Context) {
		if err := stt.EnsureAssets(ctx, sttDir, httpClient, sttConfig.SherpaVersion, modelURL, sttConfig.PunctModelURL); err != nil {
			slog.Warn("stt: asset download failed, keeping mock engine", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := stt.LoadLibrary(libPath); err != nil {
			slog.Warn("stt: library load failed after download, keeping mock engine", "err", err)
			return
		}

		modelDir := stt.ModelDir(sttDir)
		slog.Info("stt: real engine active (sherpa-onnx SenseVoice)",
			"model_dir", modelDir,
			"hq", useHQ,
			"model_url", modelURL,
		)
	})
}

// InitTTSEngine 初始化 TTS Provider 并注入 ChatHandler。
//
// 三条路径由 ttsConfig.Provider 决定：
//   - "edge"    → EdgeProvider（Microsoft Edge TTS WebSocket，无需下载，立即可用）
//   - "http"    → HTTPProvider（外部 sidecar，如 CosyVoice 2 / Qwen3-TTS）
//   - ""/"sherpa" → SherpaProvider（sherpa-onnx 本地 Kokoro，异步下载后激活）
func (s *Server) InitTTSEngine(ctx context.Context, dataDir string, gate *probe.FeatureGate, httpClient *http.Client, ttsConfig config.TTSConfig) {
	switch ttsConfig.Provider {
	case "edge":
		// Edge TTS：免费、无需下载、立即激活，不受 FeatureGate 门控（无内存开销）
		p := tts.NewEdgeProvider(ttsConfig.EdgeVoice)
		s.chatHandler.SetTTSEngine(p)
		slog.Info("tts: Edge TTS active", "voice", ttsConfig.EdgeVoice)
		return

	case "http":
		// HTTP sidecar：同样立即激活，连通性由首次调用时发现
		if ttsConfig.HTTPEndpoint == "" {
			slog.Warn("tts: provider=http but http_endpoint is empty, TTS disabled")
			return
		}
		p := tts.NewHTTPProvider(ttsConfig.HTTPEndpoint, httpClient)
		s.chatHandler.SetTTSEngine(p)
		slog.Info("tts: HTTP sidecar TTS active", "endpoint", ttsConfig.HTTPEndpoint)
		return
	}

	// ── Sherpa 本地路径（provider="" 或 "sherpa"）──────────────────────────────
	// 修复 bug：原代码错误使用 FeatureLocalSTT 门控 TTS，现改为独立的 FeatureLocalTTS。
	if gate != nil && gate.State(probe.FeatureLocalTTS) == probe.FeatureDisabled {
		slog.Info("tts: FeatureLocalTTS disabled by FeatureGate (need ≥512MB free)")
		return
	}
	if ttsConfig.ModelURL == "" {
		slog.Info("tts: sherpa provider but model_url is empty, TTS disabled")
		return
	}

	ttsDir := filepath.Join(dataDir, "models", "kokoro")
	concurrent.SafeGo(ctx, "gateway.server.tts_asset_download", func(context.Context) {
		sttDir := filepath.Join(dataDir, "models", "sensevoice")
		if err := tts.EnsureAssets(ctx, sttDir, ttsDir, httpClient, ttsConfig.SherpaVersion, ttsConfig.ModelURL); err != nil {
			slog.Warn("tts: asset download failed", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := tts.LoadLibrary(libPath); err != nil {
			slog.Warn("tts: library load failed", "err", err)
			return
		}

		modelDir := tts.ModelDir(ttsDir)
		engine, err := tts.NewEngine(modelDir)
		if err != nil {
			slog.Warn("tts: engine init failed", "err", err)
			return
		}
		s.chatHandler.SetTTSEngine(engine)
		slog.Info("tts: sherpa-onnx Kokoro active", "model_dir", modelDir)
	})
}

func (s *Server) setupWebUI(mux *http.ServeMux) {
	// 挂载 Web UI 静态资源：DEV_MODE=1 反代 Vite，否则用 go:embed dist
	if os.Getenv("DEV_MODE") == "1" {
		target, _ := url.Parse("http://localhost:5173")
		proxy := httputil.NewSingleHostReverseProxy(target)
		mux.Handle("/", proxy)
		return
	}

	subFS, err := fs.Sub(webui.WebUIFS, "dist")
	if err != nil {
		return
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Don't fallback for API routes
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		// Clean the path to check if it exists in the embed FS
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "."
		}

		// Check if the requested file exists
		f, err := subFS.Open(p)
		if err != nil {
			// Fallback to index.html for SPA routing
			r.URL.Path = "/"
		} else {
			f.Close()
		}

		// 缓存策略与字符编码：
		// - index.html 及所有 HTML：no-cache（每次重新验证，防止浏览器用旧 HTML）
		// - /assets/*.js /assets/*.css（Vite 内容 hash 命名）：immutable 永久缓存
		// - 其他静态资源：1h 缓存
		switch {
		case strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" || r.URL.Path == "":
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			} else if strings.HasSuffix(r.URL.Path, ".css") {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			}
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
			if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			}
		}

		http.FileServer(http.FS(subFS)).ServeHTTP(w, r)
	})
}

//nolint:nestif
func (s *Server) bootMarketplaceInit(ctx context.Context) {
	slog.Info("polaris-server: auto-syncing marketplaces...")
	if s.pluginHandler != nil {
		count, err := s.pluginHandler.SyncAllMarketplaces(ctx, false)
		if err != nil {
			slog.Warn("polaris-server: auto-sync marketplaces failed", "err", err)
		} else {
			slog.Info("polaris-server: auto-sync marketplaces finished", "synced_count", count)
		}
	}
}
