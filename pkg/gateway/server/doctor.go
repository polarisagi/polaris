package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
)

// GET /v1/doctor
// 系统健康检查：数据库、FTS5 索引、Provider 配置、磁盘、内存。
// 返回 check 列表，任意 check 失败时 HTTP 200 但 "ok": false。
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var checks []check
	allOK := true

	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
		if !ok {
			allOK = false
		}
	}

	// ── 数据库连通性 ────────────────────────────────────────────────────
	var sessCount, msgCount int
	dbOK := true
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_sessions`).Scan(&sessCount); err != nil {
		dbOK = false
		add("database", false, fmt.Sprintf("query failed: %v", err))
	} else {
		s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_messages`).Scan(&msgCount) //nolint:errcheck
		add("database", true, fmt.Sprintf("ok  ·  %d sessions, %d messages", sessCount, msgCount))
	}

	// ── FTS5 全文索引 ────────────────────────────────────────────────────
	if dbOK { //nolint:nestif
		var ftsCount int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts`).Scan(&ftsCount); err != nil {
			add("fts5", false, fmt.Sprintf("messages_fts not available: %v", err))
		} else {
			syncOK := ftsCount >= msgCount // 允许摘要类消息略多
			if syncOK {
				add("fts5", true, fmt.Sprintf("ok  ·  %d indexed entries", ftsCount))
			} else {
				add("fts5", false, fmt.Sprintf("index out of sync: %d fts vs %d messages", ftsCount, msgCount))
			}
		}
	}

	// ── Provider 配置 ─────────────────────────────────────────────────
	defaultP := s.registry.PickProvider("default")
	generalP := s.registry.PickProvider("general")
	if defaultP != nil || generalP != nil {
		which := "default"
		if defaultP == nil {
			which = "general"
		}
		add("provider", true, fmt.Sprintf("active role: %s", which))
	} else {
		add("provider", false, "no enabled provider (add one in 模型 page)")
	}

	// ── Cron 任务 ─────────────────────────────────────────────────────
	var cronEnabled, cronTotal int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE enabled=1`).Scan(&cronEnabled) //nolint:errcheck
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&cronTotal)                   //nolint:errcheck
	add("cron", true, fmt.Sprintf("%d/%d jobs enabled", cronEnabled, cronTotal))

	// ── 内存 ──────────────────────────────────────────────────────────
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sysMB := ms.Sys / (1024 * 1024)
	memOK := sysMB < 7168 // < 7 GB → 在 8 GB floor 安全线内
	add("memory", memOK, fmt.Sprintf("%d MB / 8192 MB", sysMB))

	// ── 数据目录可写性 ────────────────────────────────────────────────
	if s.dataDir != "" {
		probe := s.dataDir + "/.doctor_probe"
		if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
			add("data_dir", false, fmt.Sprintf("not writable: %v", err))
		} else {
			os.Remove(probe) //nolint:errcheck
			add("data_dir", true, fmt.Sprintf("writable: %s", s.dataDir))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"ok":     allOK,
		"checks": checks,
	})
}
