package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestDocIndexAndTranslationTaskRepo(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	ddl, err := schema.FS.ReadFile("019_extension_catalog.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}

	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	// 1. 测试集中文档索引读写
	entries := []types.ExtDocIndexRow{
		{
			CatalogID:        "mp1/p1",
			MarketplaceID:    "mp1",
			DocumentKey:      "key1",
			SourceRepoURL:    "https://github.com/example/repo.git",
			SourceRevision:   "commit1",
			SourcePath:       "README.md",
			SourceKind:       "readme",
			SourceSHA256:     "sha_src_1",
			ZhPath:           "zh-CN/mp1/key1/README_zh.md",
			ZhSHA256:         "sha_zh_1",
			ZhOrigin:         "upstream",
			ZhReview:         "reviewed",
			ZhSourceSHA256:   "sha_src_1",
			GeneratorVersion: "v1",
		},
	}

	if err := r.ReplaceDocIndex(ctx, entries, map[string]string{"etag": `"doc-v1"`}); err != nil {
		t.Fatalf("ReplaceDocIndex failed: %v", err)
	}

	meta, err := r.GetDocIndexMeta(ctx, "etag")
	if err != nil || meta != `"doc-v1"` {
		t.Fatalf("unexpected meta: %q %v", meta, err)
	}

	doc, err := r.GetDocIndexEntry(ctx, "mp1/p1")
	if err != nil || doc == nil {
		t.Fatalf("expected doc entry, got %v, err=%v", doc, err)
	}
	if doc.ZhPath != "zh-CN/mp1/key1/README_zh.md" || doc.SourceSHA256 != "sha_src_1" {
		t.Fatalf("doc fields mismatch: %+v", doc)
	}

	// 查不存在的条目
	missingDoc, err := r.GetDocIndexEntry(ctx, "nonexistent")
	if err != nil || missingDoc != nil {
		t.Fatalf("expected nil for missing doc, got %v, err=%v", missingDoc, err)
	}

	// 2. 测试翻译任务持久化
	task := types.ExtDocTranslationTaskRow{
		TaskID:           "task_1",
		PrincipalID:      "user_1",
		CatalogID:        "mp_custom/custom_tool",
		SourceSHA256:     "sha_custom_1",
		Locale:           "zh-CN",
		GeneratorVersion: "v1",
		Status:           "queued",
		RetryAfter:       5,
		Attempts:         1,
	}

	if err := r.UpsertDocTranslationTask(ctx, task); err != nil {
		t.Fatalf("UpsertDocTranslationTask failed: %v", err)
	}

	// 按 task_id 查
	gotTask, err := r.GetDocTranslationTask(ctx, "task_1")
	if err != nil || gotTask == nil {
		t.Fatalf("GetDocTranslationTask failed: %v, task=%v", err, gotTask)
	}
	if gotTask.Status != "queued" || gotTask.PrincipalID != "user_1" {
		t.Fatalf("task fields mismatch: %+v", gotTask)
	}

	// 状态更新
	if err := r.UpdateDocTranslationTaskStatus(ctx, "task_1", "ready", "", 0, "/path/to/cache"); err != nil {
		t.Fatalf("UpdateDocTranslationTaskStatus failed: %v", err)
	}
	gotTaskReady, err := r.GetDocTranslationTask(ctx, "task_1")
	if err != nil || gotTaskReady == nil || gotTaskReady.Status != "ready" {
		t.Fatalf("expected ready status, got: %+v err=%v", gotTaskReady, err)
	}

	// 3. 测试失败重试时 task_id 身份更新与状态流转（A7 回归：真实 SQLite 验证）
	retryTask := task
	retryTask.TaskID = "task_retry_2"
	retryTask.Status = "queued"
	retryTask.Attempts = 2
	if err := r.UpsertDocTranslationTask(ctx, retryTask); err != nil {
		t.Fatalf("UpsertDocTranslationTask retry failed: %v", err)
	}

	// 必须能用新的 task_id 查到该记录
	gotRetry, err := r.GetDocTranslationTask(ctx, "task_retry_2")
	if err != nil || gotRetry == nil {
		t.Fatalf("expected to find retry task by new ID task_retry_2, got err=%v, task=%v", err, gotRetry)
	}
	if gotRetry.Status != "queued" || gotRetry.Attempts != 2 {
		t.Fatalf("retry task status/attempts mismatch: %+v", gotRetry)
	}

	// 必须能用新的 task_id 成功更新任务状态为 ready
	if err := r.UpdateDocTranslationTaskStatus(ctx, "task_retry_2", "ready", "", 0, "/path/to/result.md"); err != nil {
		t.Fatalf("UpdateDocTranslationTaskStatus with new ID failed: %v", err)
	}
	gotUpdated, err := r.GetDocTranslationTask(ctx, "task_retry_2")
	if err != nil || gotUpdated == nil || gotUpdated.Status != "ready" {
		t.Fatalf("expected updated status ready, got: %+v err=%v", gotUpdated, err)
	}
}
