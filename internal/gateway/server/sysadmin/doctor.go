package sysadmin

import (
	"github.com/polarisagi/polaris/internal/memory/graph"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
)

// GET /v1/doctor
// 系统健康检查：数据库、FTS5 索引、Provider 配置、磁盘、内存。
// 返回 check 列表，任意 check 失败时 HTTP 200 但 "ok": false。
func (h *SysAdminHandler) HandleDoctor(w http.ResponseWriter, r *http.Request) {
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
	if err := h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_sessions`).Scan(&sessCount); err != nil {
		dbOK = false
		add("database", false, fmt.Sprintf("query failed: %v", err))
	} else {
		h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_messages`).Scan(&msgCount) //nolint:errcheck
		add("database", true, fmt.Sprintf("ok  ·  %d sessions, %d messages", sessCount, msgCount))
	}

	// ── FTS5 全文索引 ────────────────────────────────────────────────────
	if dbOK { //nolint:nestif
		var ftsCount int
		if err := h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts`).Scan(&ftsCount); err != nil {
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

	// ── 记忆引擎统计 ────────────────────────────────────────────────────
	if h.Agent != nil && h.Agent.Memory() != nil {
		if stats, err := h.Agent.Memory().StoreStats(); err == nil && stats != "{}" {
			add("memory_backend", true, stats)
		}
	}

	// ── Provider 配置 ─────────────────────────────────────────────────
	defaultP := h.Registry.PickProvider("default")
	generalP := h.Registry.PickProvider("general")
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
	h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE enabled=1`).Scan(&cronEnabled) //nolint:errcheck
	h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&cronTotal)                   //nolint:errcheck
	add("cron", true, fmt.Sprintf("%d/%d jobs enabled", cronEnabled, cronTotal))

	// ── 内存 ──────────────────────────────────────────────────────────
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sysMB := ms.Sys / (1024 * 1024)
	memOK := sysMB < 7168 // < 7 GB → 在 8 GB floor 安全线内
	add("memory", memOK, fmt.Sprintf("%d MB / 8192 MB", sysMB))

	// ── 数据目录可写性 ────────────────────────────────────────────────
	if h.DataDir != "" {
		probe := h.DataDir + "/.doctor_probe"
		if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
			add("data_dir", false, fmt.Sprintf("not writable: %v", err))
		} else {
			os.Remove(probe) //nolint:errcheck
			add("data_dir", true, fmt.Sprintf("writable: %s", h.DataDir))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"ok":     allOK,
		"checks": checks,
	})
}

func (h *SysAdminHandler) HandleGetMMDCanvas(w http.ResponseWriter, r *http.Request) {
	canvas := graph.NewTaskMermaidCanvas()
	// Currently it might be empty if we just instantiate it, but we expose the endpoint.
	// We can hook it up later.
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(canvas.Render()))
}
