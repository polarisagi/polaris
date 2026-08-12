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
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
	webui "github.com/polarisagi/polaris/web"
)

// SeedBuiltinConfig 将 embedded yaml 配置作为种子数据写入数据库（INSERT OR IGNORE）。
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
				if err := s.extRepo.SeedCatalogEntry(context.Background(), types.ExtCatalogRow{
					ID:            e.ID,
					MarketplaceID: "builtin",
					Type:          e.Type,
					Name:          e.Name,
					Description:   e.Description,
					Publisher:     e.Publisher,
					TrustTier:     e.TrustTier,
					URL:           e.URL,
					Payload:       string(payload),
				}); err != nil {
					// 与上方 SeedMarketplace 分支保持一致的错误留痕（HE-1）。
					slog.Error("polaris-server: SeedCatalogEntry failed", "err", err, "id", e.ID)
				}
			}
		} else {
			slog.Warn("polaris-server: configs/extensions/registry.yaml parse failed", "err", err)
		}
	} else {
		slog.Warn("polaris-server: configs/extensions/registry.yaml load failed (empty)")
	}
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
