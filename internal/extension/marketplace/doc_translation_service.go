package marketplace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	maxConcurrentTranslations = 2
	translationQueueSize      = 100
	translationTimeout        = 90 * time.Second
	defaultGeneratorVersion   = "v1"
)

// DocTranslationService 自定义扩展中文翻译持久任务调度服务（State-in-DB + HE-1 可观测）。
type DocTranslationService struct {
	repo     protocol.ExtensionRepository
	locator  *DocLocator
	provider protocol.Provider
	dataDir  string

	taskCh   chan types.ExtDocTranslationTaskRow
	inFlight sync.Map // key: principalID+":"+catalogID+":"+sourceSHA256+":"+locale -> struct{}{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
}

func NewDocTranslationService(repo protocol.ExtensionRepository, locator *DocLocator, provider protocol.Provider, dataDir string) *DocTranslationService {
	return &DocTranslationService{
		repo:     repo,
		locator:  locator,
		provider: provider,
		dataDir:  dataDir,
		taskCh:   make(chan types.ExtDocTranslationTaskRow, translationQueueSize),
	}
}

// SetProvider 动态注入或切换 LLM Provider。
func (s *DocTranslationService) SetProvider(p protocol.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = p
}

func (s *DocTranslationService) getProvider() protocol.Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

// Start 启动后台消费线程，并从数据库恢复待处理任务。
func (s *DocTranslationService) Start(parentCtx context.Context) {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(parentCtx)
	s.mu.Unlock()

	s.recoverPendingTasks(s.ctx)
	s.startWorkers(s.ctx)
	s.startPoller(s.ctx)
}

func (s *DocTranslationService) enqueueOrExhaustTask(ctx context.Context, task types.ExtDocTranslationTaskRow) bool {
	if task.Attempts >= 3 {
		if err := s.repo.UpdateDocTranslationTaskStatus(ctx, task.TaskID, "failed", "attempts_exhausted", 0, ""); err != nil {
			slog.Warn("doc_translation: failed to mark task exhausted", "task_id", task.TaskID, "err", err)
		}
		return false
	}
	select {
	case s.taskCh <- task:
		return true
	default:
		return false
	}
}

func (s *DocTranslationService) recoverPendingTasks(ctx context.Context) {
	if s.repo == nil {
		return
	}
	for {
		pending, err := s.repo.ListPendingDocTranslationTasks(ctx, 50)
		if err != nil || len(pending) == 0 {
			break
		}
		enqueuedAny := false
		for _, task := range pending {
			if s.enqueueOrExhaustTask(ctx, task) {
				enqueuedAny = true
			}
		}
		if !enqueuedAny || len(pending) < 50 {
			break
		}
	}
}

func (s *DocTranslationService) startWorkers(ctx context.Context) {
	for i := 0; i < maxConcurrentTranslations; i++ {
		s.wg.Add(1)
		concurrent.SafeGo(ctx, "doc_translation_worker", func(workerCtx context.Context) {
			defer s.wg.Done()
			for {
				select {
				case <-workerCtx.Done():
					return
				case task, ok := <-s.taskCh:
					if !ok {
						return
					}
					s.processTask(workerCtx, task)
				}
			}
		})
	}
}

func (s *DocTranslationService) startPoller(ctx context.Context) {
	s.wg.Add(1)
	concurrent.SafeGo(ctx, "doc_translation_poller", func(pollerCtx context.Context) {
		defer s.wg.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				s.pollPendingTasks(pollerCtx)
			}
		}
	})
}

func (s *DocTranslationService) pollPendingTasks(ctx context.Context) {
	if s.repo == nil {
		return
	}
	pending, err := s.repo.ListPendingDocTranslationTasks(ctx, 20)
	if err != nil {
		return
	}
	for _, task := range pending {
		if task.Status != "queued" {
			continue
		}
		if !s.enqueueOrExhaustTask(ctx, task) && task.Attempts < 3 {
			// 队列满则等待下一轮
			break
		}
	}
}

// Stop 停止翻译服务并等待活跃任务收尾。
func (s *DocTranslationService) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *DocTranslationService) validateTranslationTarget(ctx context.Context, catalogID, sourceSHA256 string) (*LocatedSource, error) {
	isBuiltin, err := s.locator.IsBuiltin(ctx, catalogID)
	if err != nil {
		return nil, err
	}
	if isBuiltin {
		return nil, apperr.New(apperr.CodeForbidden, "builtin_translation_managed")
	}

	src, err := s.locator.LocateSource(ctx, catalogID)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, apperr.New(apperr.CodeNotFound, "source document not found")
	}
	if !strings.EqualFold(src.SHA256, sourceSHA256) {
		return nil, apperr.New(apperr.CodeConflict, "source_changed")
	}
	return src, nil
}

// EnqueueTranslation 接收用户显式翻译请求，严格鉴权、校验源哈希并幂等入队。
func (s *DocTranslationService) EnqueueTranslation(ctx context.Context, principalID, catalogID, locale, sourceSHA256 string) (*types.ExtDocTranslationTaskRow, error) {
	ctx, span := otel.Tracer("doc_translation").Start(ctx, "DocTranslationService.EnqueueTranslation")
	defer span.End()

	if principalID == "" {
		principalID = "anonymous"
	}
	if locale == "" {
		locale = "zh-CN"
	}
	if locale != "zh-CN" {
		return nil, apperr.New(apperr.CodeInvalidInput, "unsupported translation locale: "+locale)
	}

	src, err := s.validateTranslationTarget(ctx, catalogID, sourceSHA256)
	if err != nil {
		return nil, err
	}

	// 幂等查重（授权主体 + catalog_id + source_sha256 + locale + generator_version）
	if s.repo != nil {
		existing, err := s.repo.FindDocTranslationTask(ctx, principalID, catalogID, src.SHA256, locale, defaultGeneratorVersion)
		if err == nil && existing != nil {
			if existing.Status == "ready" || existing.Status == "queued" || existing.Status == "translating" {
				return existing, nil
			}
		}
	}

	// 创建新持久任务
	taskID := newTranslationTaskID()
	task := types.ExtDocTranslationTaskRow{
		TaskID:           taskID,
		PrincipalID:      principalID,
		CatalogID:        catalogID,
		SourceSHA256:     src.SHA256,
		Locale:           locale,
		GeneratorVersion: defaultGeneratorVersion,
		Status:           "queued",
		RetryAfter:       5,
		Attempts:         1,
	}

	if s.repo != nil {
		if err := s.repo.UpsertDocTranslationTask(ctx, task); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "doc_translation: upsert task", err)
		}
	}

	metrics.RecordExtensionDocTranslation(ctx, "queued", "")

	// 投递后台任务队列
	select {
	case s.taskCh <- task:
	default:
		// 队列满时保持在 DB 的 queued 状态，等 worker 轮询
	}

	return &task, nil
}

// GetTask 获取指定主体的任务状态。严格主体隔离，不泄露其他用户任务。
func (s *DocTranslationService) GetTask(ctx context.Context, principalID, taskID string) (*types.ExtDocTranslationTaskRow, error) {
	if s.repo == nil {
		return nil, apperr.New(apperr.CodeNotFound, "task not found")
	}
	task, err := s.repo.GetDocTranslationTask(ctx, taskID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "doc_translation: get task", err)
	}
	if task == nil {
		return nil, apperr.New(apperr.CodeNotFound, "task not found")
	}
	// 鉴权隔离
	if principalID != "" && task.PrincipalID != principalID && task.PrincipalID != "anonymous" {
		return nil, apperr.New(apperr.CodeNotFound, "task not found")
	}
	return task, nil
}

func (s *DocTranslationService) runTranslationInfer(ctx context.Context, provider protocol.Provider, content string) (*types.ProviderResponse, error) {
	systemPrompt := `You are an accurate, faithful technical documentation translator.
Translate the provided English Markdown into Simplified Chinese (zh-CN).
Strict rules:
1. Preserve all headings, lists, tables, links, images, and formatting exactly.
2. DO NOT translate code blocks (` + "```" + `...` + "```" + `), inline code (` + "`" + `...` + "`" + `), terminal commands, CLI flags, configuration keys, identifiers, environment variables, or URLs.
3. Output ONLY the translated Markdown directly. Do not include markdown code block wrapper around the whole document, do not add introductory text, conversational preamble, or personal notes.
4. Never invent installation steps, permissions, or system capabilities not mentioned in the source.`

	msgs := []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: content},
	}

	resp, err := safecall.Infer(ctx, provider, msgs,
		types.WithPurpose(types.PurposeDocTranslate),
		types.WithThinkingMode(types.ThinkingDisabled),
		types.WithTemperature(0.2),
		types.WithMaxTokens(4096),
	)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "doc_translation: inference", err)
	}
	return resp, nil
}

func (s *DocTranslationService) writeTranslationCache(cachePath, content string) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "doc_translation: mkdir cache", err)
	}
	tmpFile := cachePath + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "doc_translation: write cache tmp", err)
	}
	if err := os.Rename(tmpFile, cachePath); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "doc_translation: rename cache", err)
	}
	return nil
}

func (s *DocTranslationService) validateTaskPreconditions(ctx context.Context, task types.ExtDocTranslationTaskRow) (*LocatedSource, protocol.Provider, string) {
	isBuiltin, err := s.locator.IsBuiltin(ctx, task.CatalogID)
	if err != nil || isBuiltin {
		return nil, nil, "builtin_translation_managed"
	}

	provider := s.getProvider()
	if provider == nil {
		return nil, nil, "provider_unavailable"
	}

	src, err := s.locator.LocateSource(ctx, task.CatalogID)
	if err != nil || src == nil || !strings.EqualFold(src.SHA256, task.SourceSHA256) {
		return nil, nil, "source_changed"
	}
	return src, provider, ""
}

func validateInferResponse(resp *types.ProviderResponse, err error) (string, string) {
	if err != nil || resp == nil || strings.TrimSpace(resp.Content) == "" {
		if err != nil && strings.Contains(err.Error(), "budget") {
			return "", "budget_exceeded"
		}
		return "", "translation_failed"
	}
	if resp.FinishReason == "length" {
		return "", "content_length_exceeded"
	}
	return strings.TrimSpace(resp.Content), ""
}

func (s *DocTranslationService) completeTask(ctx context.Context, task types.ExtDocTranslationTaskRow, translatedMarkdown string) {
	latestSrc, err := s.locator.LocateSource(ctx, task.CatalogID)
	if err != nil || latestSrc == nil || !strings.EqualFold(latestSrc.SHA256, task.SourceSHA256) {
		s.failTask(task.TaskID, "source_changed")
		return
	}

	cachePath := s.cacheFilePath(task.PrincipalID, task.CatalogID, task.SourceSHA256, task.Locale)
	if err := s.writeTranslationCache(cachePath, translatedMarkdown); err != nil {
		s.failTask(task.TaskID, "cache_write_failed")
		return
	}

	if s.repo != nil {
		dbCtx, cancelDB := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.repo.UpdateDocTranslationTaskStatus(dbCtx, task.TaskID, "ready", "", 0, cachePath); err != nil {
			slog.Warn("doc_translation: failed to mark task ready", "task_id", task.TaskID, "err", err)
		}
		cancelDB()
	}
	metrics.RecordExtensionDocTranslation(ctx, "ready", "")
}

func (s *DocTranslationService) processTask(rootCtx context.Context, task types.ExtDocTranslationTaskRow) {
	dedupKey := task.PrincipalID + ":" + task.CatalogID + ":" + task.SourceSHA256 + ":" + task.Locale
	if _, loaded := s.inFlight.LoadOrStore(dedupKey, struct{}{}); loaded {
		// 同键并发已有 goroutine 在执行，合并返回
		return
	}
	defer s.inFlight.Delete(dedupKey)

	ctx, cancel := context.WithTimeout(rootCtx, translationTimeout)
	defer cancel()

	ctx, span := otel.Tracer("doc_translation").Start(ctx, "DocTranslationService.processTask")
	defer span.End()

	startTime := time.Now()

	// 标记状态为 translating 并递增 attempts
	task.Attempts++
	if s.repo != nil {
		if err := s.repo.UpdateDocTranslationTaskStatus(ctx, task.TaskID, "translating", "", 5, ""); err != nil {
			slog.Warn("doc_translation: failed to mark task translating", "task_id", task.TaskID, "err", err)
		}
	}

	src, provider, errCode := s.validateTaskPreconditions(ctx, task)
	if errCode != "" {
		s.failTask(task.TaskID, errCode)
		return
	}

	resp, err := s.runTranslationInfer(ctx, provider, src.Content)
	duration := time.Since(startTime).Seconds()
	metrics.RecordExtensionDocTranslationDuration(ctx, duration)

	translated, inferErrCode := validateInferResponse(resp, err)
	if inferErrCode != "" {
		s.failTask(task.TaskID, inferErrCode)
		return
	}

	s.completeTask(ctx, task, translated)
}

func (s *DocTranslationService) failTask(taskID, errorCode string) {
	if s.repo != nil {
		dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.repo.UpdateDocTranslationTaskStatus(dbCtx, taskID, "failed", errorCode, 0, ""); err != nil {
			slog.Warn("doc_translation: failed to mark task failed", "task_id", taskID, "err", err)
		}
		cancel()
	}
	metrics.RecordExtensionDocTranslation(context.Background(), "failed", errorCode)
}

func (s *DocTranslationService) cacheFilePath(principalID, catalogID, sourceSHA, locale string) string {
	principalHash := sha256Hex(principalID)
	catHash := sha256Hex(catalogID)
	fileName := sourceSHA + "_" + locale + ".md"
	return filepath.Join(s.dataDir, "extensions", "cache", "doc_translations", principalHash, catHash, fileName)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newTranslationTaskID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("task_trans_%x", time.Now().UnixNano())
	}
	return "task_trans_" + hex.EncodeToString(b)
}
