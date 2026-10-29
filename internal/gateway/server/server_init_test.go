package server

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestServerInitExtra(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sys_config (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS plugin_marketplaces (
			id TEXT PRIMARY KEY,
			name TEXT,
			repo_url TEXT,
			description TEXT,
			type TEXT,
			is_builtin BOOLEAN,
			trust_tier INTEGER,
			enabled BOOLEAN,
			sort_order INTEGER,
			publisher TEXT,
			status TEXT,
			last_sync_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		db:           db,
		chatRepo:     repo.NewSQLiteChatRepository(db),
		extRepo:      repo.NewSQLiteExtensionRepository(db),
		providerRepo: repo.NewSQLiteProviderRepository(db),
		installMgr:   marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil, nil),
	}
	s.SeedBuiltinConfig(nil, nil)
	s.bootMarketplaceInit(context.Background())
	// 原先此处还调用 s.InitSTTEngine / s.InitTTSEngine。二者与
	// cmd/polaris/server_stt_tts.go 的 initSTTEngine/initTTSEngine 逐行重复，
	// 而生产启动只走后者；2026-08-12 复核确认前者零生产调用方（唯一调用点就是
	// 本测试），且其 Edge TTS 分支构造 Provider 时 SafeDialer 传 nil——一旦有人
	// 误接线，TTS 出站会绕过 SSRFGuard。已整体删除，STT/TTS 初始化的覆盖由
	// cmd/polaris 侧承担。
}
