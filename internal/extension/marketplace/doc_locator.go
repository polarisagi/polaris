package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.opentelemetry.io/otel"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// maxDocBytes 单个 Markdown 说明文档读取上限（2MB）。
const maxDocBytes = 2 << 20

// DocSourceInfo 文档源数据契约。
type DocSourceInfo struct {
	URL      string `json:"url"`
	Revision string `json:"revision"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	SHA256   string `json:"sha256"`
	BaseURL  string `json:"base_url"`
}

// DocTranslationInfo 译文元数据契约。
type DocTranslationInfo struct {
	Origin           string `json:"origin"`
	Review           string `json:"review"`
	SourceSHA256     string `json:"source_sha256"`
	GeneratorVersion string `json:"generator_version"`
}

// DocumentationResponse 文档 API 返回结构（与 CONTRACT.md 完全一致）。
type DocumentationResponse struct {
	CatalogID       string              `json:"catalog_id"`
	RequestedLocale string              `json:"requested_locale"`
	ResolvedLocale  *string             `json:"resolved_locale"`
	Status          string              `json:"status"`
	Markdown        string              `json:"markdown"`
	Source          *DocSourceInfo      `json:"source"`
	Translation     *DocTranslationInfo `json:"translation"`
	Stale           bool                `json:"stale"`
	FallbackReason  *string             `json:"fallback_reason"`
	TaskID          *string             `json:"task_id"`
}

// LocatedSource 本地或上游解析出的原始文档。
type LocatedSource struct {
	Content  string
	SHA256   string
	Info     DocSourceInfo
	ZhPath   string
	ZhBytes  []byte
	IsSkill  bool
	Builtin  bool
	LocalDir string
}

// DocLocator 负责多源文档定位与按优先级解析。
type DocLocator struct {
	repo       protocol.ExtensionRepository
	http       network.SafeHTTPClient
	dataDir    string
	cacheDir   string
	installDir string
}

func NewDocLocator(repo protocol.ExtensionRepository, httpClient network.SafeHTTPClient, dataDir, cacheDir, installDir string) *DocLocator {
	if cacheDir == "" {
		cacheDir = filepath.Join(dataDir, "cache")
	}
	if installDir == "" {
		installDir = filepath.Join(dataDir, "extensions", "installed")
	}
	return &DocLocator{
		repo:       repo,
		http:       httpClient,
		dataDir:    dataDir,
		cacheDir:   cacheDir,
		installDir: installDir,
	}
}

// IsBuiltin 判断该 catalog_id 是否属于内置扩展（官方或预置市场）。
// 查询错误时 fail-closed 返回错误，绝不静默放行推断为自定义扩展。
func (l *DocLocator) IsBuiltin(ctx context.Context, catalogID string) (bool, error) {
	if strings.HasPrefix(catalogID, "polarisagi/polaris-plugins-official") ||
		strings.HasPrefix(catalogID, "builtin/") {
		return true, nil
	}
	if l.repo == nil {
		return false, apperr.New(apperr.CodeInternal, "repo unavailable")
	}
	if isBuiltin, found, err := l.isBuiltinCatalogEntry(ctx, catalogID); err != nil || found {
		return isBuiltin, err
	}
	return l.isBuiltinInstance(ctx, catalogID)
}

func (l *DocLocator) isBuiltinCatalogEntry(ctx context.Context, catalogID string) (bool, bool, error) {
	catEntry, err := l.repo.GetCatalogEntry(ctx, catalogID)
	if err != nil {
		return false, false, apperr.Wrap(apperr.CodeInternal, "doc_locator: get catalog entry", err)
	}
	if catEntry != nil {
		mp, mpErr := l.repo.GetMarketplace(ctx, catEntry.MarketplaceID)
		if mpErr != nil {
			return false, false, apperr.Wrap(apperr.CodeInternal, "doc_locator: get marketplace", mpErr)
		}
		if mp != nil {
			return mp.IsBuiltin == 1, true, nil
		}
	}
	return false, false, nil
}

func (l *DocLocator) isBuiltinInstance(ctx context.Context, catalogID string) (bool, error) {
	instances, err := l.repo.ListInstances(ctx)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "doc_locator: list instances", err)
	}
	for _, inst := range instances {
		if inst.CatalogID != catalogID && inst.ID != catalogID {
			continue
		}
		if inst.Origin == "builtin" {
			return true, nil
		}
		if inst.Origin == "marketplace" {
			isBuiltin, err := l.isMarketplaceBuiltin(ctx, catalogID)
			if err != nil || isBuiltin {
				return isBuiltin, err
			}
		}
	}
	return false, nil
}

func (l *DocLocator) isMarketplaceBuiltin(ctx context.Context, catalogID string) (bool, error) {
	parts := strings.Split(catalogID, "/")
	if len(parts) < 2 {
		return false, nil
	}
	mpID := parts[0] + "/" + parts[1]
	mp, err := l.repo.GetMarketplace(ctx, mpID)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "doc_locator: get marketplace for instance", err)
	}
	return mp != nil && mp.IsBuiltin == 1, nil
}

// LocateSource 定位扩展的原始英文/源文档。支持未安装与已安装扩展。
func (l *DocLocator) LocateSource(ctx context.Context, catalogID string) (*LocatedSource, error) {
	_, span := otel.Tracer("doc_locator").Start(ctx, "DocLocator.LocateSource")
	defer span.End()

	isBuiltin, err := l.IsBuiltin(ctx, catalogID)
	if err != nil {
		// 查询内置状态失败时，若属于数据库不可用等错误，直接 fail-closed
		return nil, err
	}

	// 1. 优先检查是否已安装（从 install_path 读取，同时支持 catalogID 与直装实例 ID ext_xxx）
	if l.repo != nil {
		instances, err := l.repo.ListInstances(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "doc_locator: list instances for locate source", err)
		}
		for _, inst := range instances {
			if (inst.CatalogID == catalogID || inst.ID == catalogID) && inst.InstallPath != "" {
				if src := l.readFromLocalDir(inst.InstallPath, catalogID, isBuiltin); src != nil {
					return src, nil
				}
			}
		}
	}

	// 2. 未安装：从 extension_catalog 目录快照及市场本地缓存读取
	if l.repo != nil {
		catEntry, err := l.repo.GetCatalogEntry(ctx, catalogID)
		if err == nil && catEntry != nil {
			src := l.readFromCatalogEntry(catEntry, isBuiltin)
			if src != nil {
				return src, nil
			}
		}
	}

	return nil, nil
}

func isDirWithinRoot(root, dir string) bool {
	if root == "" {
		return true
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(realRoot, dir)
	return err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func findLocalDoc(dir string) (targetPath, kind string) {
	readmePath := filepath.Join(dir, "README.md")
	if fileExists(readmePath) {
		return readmePath, "readme"
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	if fileExists(skillPath) {
		return skillPath, "skill"
	}
	return "", ""
}

func readLocalBundledZh(dir, realDir string) (string, []byte) {
	zhPath := filepath.Join(dir, "README_zh.md")
	if !fileExists(zhPath) {
		return zhPath, nil
	}
	realZh, err := filepath.EvalSymlinks(zhPath)
	if err != nil {
		return zhPath, nil
	}
	relZh, err := filepath.Rel(realDir, realZh)
	if err != nil || strings.HasPrefix(relZh, "..") {
		return zhPath, nil
	}
	zhBytes, _, _ := readFileLimited(zhPath, maxDocBytes)
	return zhPath, zhBytes
}

// readFromLocalDir 从本地已安装目录安全读取说明文档。
func (l *DocLocator) readFromLocalDir(dir, catalogID string, builtin bool) *LocatedSource {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil || !isDirWithinRoot(l.dataDir, realDir) {
		return nil
	}

	targetPath, kind := findLocalDoc(dir)
	if targetPath == "" {
		return nil
	}

	realTarget, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return nil
	}
	relTarget, err := filepath.Rel(realDir, realTarget)
	if err != nil || strings.HasPrefix(relTarget, "..") || filepath.IsAbs(relTarget) {
		return nil
	}

	content, hash, err := readFileLimited(targetPath, maxDocBytes)
	if err != nil {
		return nil
	}

	zhPath, zhBytes := readLocalBundledZh(dir, realDir)
	revision := downloader.GitShortHash(dir)
	if revision == "" {
		revision = "local"
	}

	relPath, _ := filepath.Rel(dir, targetPath)
	return &LocatedSource{
		Content:  string(content),
		SHA256:   hash,
		ZhPath:   zhPath,
		ZhBytes:  zhBytes,
		IsSkill:  kind == "skill",
		Builtin:  builtin,
		LocalDir: dir,
		Info: DocSourceInfo{
			URL:      "local://" + catalogID,
			Revision: revision,
			Path:     filepath.ToSlash(relPath),
			Kind:     kind,
			SHA256:   hash,
			BaseURL:  "",
		},
	}
}

// readFromCatalogEntry 从市场同步缓存读取源文档。
func (l *DocLocator) readFromCatalogEntry(entry *types.ExtCatalogRow, builtin bool) *LocatedSource {
	var regEntry protocol.RegistryEntry
	if err := json.Unmarshal([]byte(entry.Payload), &regEntry); err != nil {
		return nil
	}

	cleanMPID := strings.ReplaceAll(entry.MarketplaceID, "/", "_")
	mpDir := filepath.Join(l.cacheDir, cleanMPID)
	if !dirExists(mpDir) {
		altDir := filepath.Join(l.cacheDir, "marketplaces", cleanMPID)
		if dirExists(altDir) {
			mpDir = altDir
		}
	}

	if entry.Type == "plugin" && len(regEntry.Entry) > 0 {
		return l.readPluginCatalogDoc(entry, regEntry, mpDir, builtin)
	}
	if entry.Type == "skill" {
		return l.readSkillCatalogDoc(entry, regEntry, mpDir, builtin)
	}
	return nil
}

func (l *DocLocator) readPluginCatalogDoc(entry *types.ExtCatalogRow, regEntry protocol.RegistryEntry, mpDir string, builtin bool) *LocatedSource {
	var me pluginspec.MarketplaceEntry
	if err := json.Unmarshal(regEntry.Entry, &me); err != nil {
		return nil
	}
	relDir := ""
	if me.Source.Type == pluginspec.SourceRelative || (me.Source.Type == pluginspec.SourceGitSubdir && me.Source.Path != "") {
		relDir = me.Source.Path
	}
	targetDir := filepath.Join(mpDir, relDir)
	if targetDir == "" || !dirExists(targetDir) {
		return nil
	}

	targetDoc, kind := findLocalDoc(targetDir)
	if targetDoc == "" {
		return nil
	}

	content, hash, err := readFileLimited(targetDoc, maxDocBytes)
	if err != nil {
		return nil
	}

	zhPath := filepath.Join(targetDir, "README_zh.md")
	var zhBytes []byte
	if fileExists(zhPath) {
		zhBytes, _, _ = readFileLimited(zhPath, maxDocBytes)
	}
	rev := downloader.GitShortHash(mpDir)
	if rev == "" {
		rev = entry.Version
	}
	docRelPath := filepath.ToSlash(filepath.Join(relDir, filepath.Base(targetDoc)))
	baseURL := ""
	if strings.HasPrefix(entry.URL, "https://github.com/") {
		ownerRepo := strings.TrimSuffix(strings.TrimPrefix(entry.URL, "https://github.com/"), ".git")
		baseURL = "https://raw.githubusercontent.com/" + ownerRepo + "/" + rev + "/" + filepath.ToSlash(relDir)
	}

	return &LocatedSource{
		Content:  string(content),
		SHA256:   hash,
		ZhPath:   zhPath,
		ZhBytes:  zhBytes,
		IsSkill:  kind == "skill",
		Builtin:  builtin,
		LocalDir: targetDir,
		Info: DocSourceInfo{
			URL:      entry.URL,
			Revision: rev,
			Path:     docRelPath,
			Kind:     kind,
			SHA256:   hash,
			BaseURL:  baseURL,
		},
	}
}

func (l *DocLocator) readSkillCatalogDoc(entry *types.ExtCatalogRow, regEntry protocol.RegistryEntry, mpDir string, builtin bool) *LocatedSource {
	targetDir := regEntry.SourceDir
	if targetDir == "" || !dirExists(targetDir) {
		targetDir = filepath.Join(mpDir, regEntry.Name)
	}
	skillPath := filepath.Join(targetDir, "SKILL.md")
	if !fileExists(skillPath) {
		return nil
	}
	content, hash, err := readFileLimited(skillPath, maxDocBytes)
	if err != nil {
		return nil
	}
	rev := downloader.GitShortHash(mpDir)
	if rev == "" {
		rev = entry.Version
	}
	zhPath := filepath.Join(targetDir, "README_zh.md")
	var zhBytes []byte
	if fileExists(zhPath) {
		zhBytes, _, _ = readFileLimited(zhPath, maxDocBytes)
	}
	return &LocatedSource{
		Content:  string(content),
		SHA256:   hash,
		ZhPath:   zhPath,
		ZhBytes:  zhBytes,
		IsSkill:  true,
		Builtin:  builtin,
		LocalDir: targetDir,
		Info: DocSourceInfo{
			URL:      entry.URL,
			Revision: rev,
			Path:     filepath.ToSlash(filepath.Join(regEntry.Name, "SKILL.md")),
			Kind:     "skill",
			SHA256:   hash,
			BaseURL:  "",
		},
	}
}

// ResolveDocumentation 按 CONTRACT 解析文档。只读操作，不触发 LLM。
func (l *DocLocator) ResolveDocumentation(ctx context.Context, catalogID, requestedLocale, principalID string) (*DocumentationResponse, error) {
	ctx, span := otel.Tracer("doc_locator").Start(ctx, "DocLocator.ResolveDocumentation")
	defer span.End()

	if requestedLocale == "" {
		requestedLocale = "zh-CN"
	}

	src, err := l.LocateSource(ctx, catalogID)
	if err != nil {
		return nil, err
	}

	// 无源文档直接返回 missing
	if src == nil {
		reason := "no_source_document"
		metrics.RecordExtensionDocRequest(ctx, "missing", "missing", requestedLocale)
		return &DocumentationResponse{
			CatalogID:       catalogID,
			RequestedLocale: requestedLocale,
			ResolvedLocale:  nil,
			Status:          "missing",
			Markdown:        "",
			Source:          nil,
			Translation:     nil,
			Stale:           false,
			FallbackReason:  &reason,
			TaskID:          nil,
		}, nil
	}

	srcInfo := src.Info

	// 1. 英文直接返回原文
	if requestedLocale == "en" {
		resLocale := "en"
		metrics.RecordExtensionDocRequest(ctx, "origin", "ready", "en")
		return &DocumentationResponse{
			CatalogID:       catalogID,
			RequestedLocale: requestedLocale,
			ResolvedLocale:  &resLocale,
			Status:          "ready",
			Markdown:        src.Content,
			Source:          &srcInfo,
			Translation:     nil,
			Stale:           false,
			FallbackReason:  nil,
			TaskID:          nil,
		}, nil
	}

	if resp := l.resolveBundledZh(ctx, catalogID, requestedLocale, src); resp != nil {
		return resp, nil
	}
	if resp := l.resolveCentralZh(ctx, catalogID, requestedLocale, src); resp != nil {
		return resp, nil
	}
	if resp := l.resolveManagedTaskZh(ctx, catalogID, requestedLocale, principalID, src); resp != nil {
		return resp, nil
	}

	// 5. 兜底展示当前原文，说明未翻译或内置维护中
	resLocale := "en"
	reason := "untranslated"
	if src.Builtin {
		reason = "builtin_translation_managed"
	}
	metrics.RecordExtensionDocRequest(ctx, "fallback_origin", "ready", "zh-CN")
	return &DocumentationResponse{
		CatalogID:       catalogID,
		RequestedLocale: requestedLocale,
		ResolvedLocale:  &resLocale,
		Status:          "ready",
		Markdown:        src.Content,
		Source:          &srcInfo,
		Translation:     nil,
		Stale:           false,
		FallbackReason:  &reason,
		TaskID:          nil,
	}, nil
}

func (l *DocLocator) resolveBundledZh(ctx context.Context, catalogID, requestedLocale string, src *LocatedSource) *DocumentationResponse {
	if len(src.ZhBytes) == 0 {
		return nil
	}
	isReviewed := false
	if l.repo != nil {
		docEntry, _ := l.repo.GetDocIndexEntry(ctx, catalogID)
		if docEntry != nil && strings.EqualFold(docEntry.ZhSourceSHA256, src.SHA256) && docEntry.ZhReview == "reviewed" {
			isReviewed = true
		}
	}
	reviewStatus := "unreviewed"
	staleStatus := true
	if isReviewed {
		reviewStatus = "reviewed"
		staleStatus = false
	}
	resLocale := "zh-CN"
	metrics.RecordExtensionDocRequest(ctx, "origin_zh", "ready", "zh-CN")
	return &DocumentationResponse{
		CatalogID:       catalogID,
		RequestedLocale: requestedLocale,
		ResolvedLocale:  &resLocale,
		Status:          "ready",
		Markdown:        string(src.ZhBytes),
		Source:          &src.Info,
		Translation: &DocTranslationInfo{
			Origin:           "bundled",
			Review:           reviewStatus,
			SourceSHA256:     "",
			GeneratorVersion: "upstream",
		},
		Stale:          staleStatus,
		FallbackReason: nil,
		TaskID:         nil,
	}
}

func (l *DocLocator) resolveCentralZh(ctx context.Context, catalogID, requestedLocale string, src *LocatedSource) *DocumentationResponse {
	if l.repo == nil {
		return nil
	}
	docEntry, err := l.repo.GetDocIndexEntry(ctx, catalogID)
	if err != nil || docEntry == nil || docEntry.ZhPath == "" || docEntry.ZhSourceSHA256 != src.SHA256 {
		return nil
	}
	zhMarkdown, fetchErr := l.fetchCentralDocContent(ctx, docEntry.ZhPath, docEntry.ZhSHA256)
	if fetchErr != nil || zhMarkdown == "" {
		return nil
	}
	resLocale := "zh-CN"
	metrics.RecordExtensionDocRequest(ctx, "index_"+docEntry.ZhOrigin, "ready", "zh-CN")
	return &DocumentationResponse{
		CatalogID:       catalogID,
		RequestedLocale: requestedLocale,
		ResolvedLocale:  &resLocale,
		Status:          "ready",
		Markdown:        zhMarkdown,
		Source:          &src.Info,
		Translation: &DocTranslationInfo{
			Origin:           docEntry.ZhOrigin,
			Review:           docEntry.ZhReview,
			SourceSHA256:     docEntry.ZhSourceSHA256,
			GeneratorVersion: docEntry.GeneratorVersion,
		},
		Stale:          false,
		FallbackReason: nil,
		TaskID:         nil,
	}
}

func (l *DocLocator) resolveManagedTaskZh(ctx context.Context, catalogID, requestedLocale, principalID string, src *LocatedSource) *DocumentationResponse {
	if l.repo == nil || principalID == "" {
		return nil
	}
	task, err := l.repo.FindDocTranslationTask(ctx, principalID, catalogID, src.SHA256, "zh-CN", "v1")
	if err != nil || task == nil {
		return nil
	}
	if task.Status == "ready" && task.ResultCachePath != "" {
		cachedBytes, _, readErr := readFileLimited(task.ResultCachePath, maxDocBytes)
		if readErr == nil && len(cachedBytes) > 0 {
			resLocale := "zh-CN"
			metrics.RecordExtensionDocRequest(ctx, "cache", "ready", "zh-CN")
			return &DocumentationResponse{
				CatalogID:       catalogID,
				RequestedLocale: requestedLocale,
				ResolvedLocale:  &resLocale,
				Status:          "ready",
				Markdown:        string(cachedBytes),
				Source:          &src.Info,
				Translation: &DocTranslationInfo{
					Origin:           "machine",
					Review:           "unreviewed",
					SourceSHA256:     task.SourceSHA256,
					GeneratorVersion: task.GeneratorVersion,
				},
				Stale:          false,
				FallbackReason: nil,
				TaskID:         nil,
			}
		}
	} else if task.Status == "queued" || task.Status == "translating" {
		metrics.RecordExtensionDocRequest(ctx, "task_pending", task.Status, "zh-CN")
		return &DocumentationResponse{
			CatalogID:       catalogID,
			RequestedLocale: requestedLocale,
			ResolvedLocale:  nil,
			Status:          task.Status,
			Markdown:        src.Content,
			Source:          &src.Info,
			Translation:     nil,
			Stale:           false,
			FallbackReason:  nil,
			TaskID:          &task.TaskID,
		}
	}
	return nil
}

// fetchCentralDocContent 读取本地或远端集中中文文档正文，并严格校验 SHA-256。
func (l *DocLocator) fetchCentralDocContent(ctx context.Context, relPath, expectedSHA string) (string, error) {
	if expectedSHA == "" || !isHex64(expectedSHA) {
		return "", apperr.New(apperr.CodeInvalidInput, "expectedSHA must be valid 64-char hex")
	}

	// 先查本地缓存
	cacheFile := filepath.Join(l.dataDir, "extensions", "cache", "central_readmes", filepath.FromSlash(relPath))
	if fileExists(cacheFile) {
		bytes, hash, err := readFileLimited(cacheFile, maxDocBytes)
		if err == nil && strings.EqualFold(hash, expectedSHA) {
			return string(bytes), nil
		}
	}

	body, err := l.downloadCentralDoc(ctx, relPath, expectedSHA)
	if err != nil {
		return "", err
	}

	// 原子写入本地缓存
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		slog.Warn("doc_locator: failed to create cache dir", "err", err)
	}
	tmpFile := cacheFile + ".tmp"
	if err := os.WriteFile(tmpFile, body, 0o644); err == nil {
		if err := os.Rename(tmpFile, cacheFile); err != nil {
			slog.Warn("doc_locator: failed to rename cache file", "err", err)
		}
	}

	return string(body), nil
}

func (l *DocLocator) downloadCentralDoc(ctx context.Context, relPath, expectedSHA string) ([]byte, error) {
	baseURL := strings.TrimSuffix(config.CatalogDocIndexURL(), "/readmes/index.json")
	fullURL := baseURL + "/readmes/" + strings.TrimPrefix(relPath, "/")
	if !strings.HasPrefix(fullURL, "https://") {
		return nil, apperr.New(apperr.CodeForbidden, "doc URL must be https")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "build doc request", err)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "fetch doc", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeNetworkUnavailable, "doc returned "+resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes+1))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "read doc", err)
	}
	if int64(len(body)) > maxDocBytes {
		return nil, apperr.New(apperr.CodeInvalidInput, "doc exceeds limit")
	}
	sum := sha256.Sum256(body)
	actualSHA := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actualSHA, expectedSHA) {
		return nil, apperr.New(apperr.CodeInvalidInput, "doc hash mismatch")
	}
	return body, nil
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func readFileLimited(path string, limit int64) ([]byte, string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInternal, "doc_locator: lstat file "+path, err)
	}
	// 安全门控：拒绝对外符号链接
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, "", apperr.New(apperr.CodeForbidden, "symlink not allowed")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInternal, "doc_locator: open file "+path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInternal, "doc_locator: read file "+path, err)
	}
	if int64(len(data)) > limit {
		return nil, "", apperr.New(apperr.CodeInvalidInput, "file exceeds limit")
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}
