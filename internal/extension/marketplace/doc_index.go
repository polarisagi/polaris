package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// DocIndexSchemaVersion readmes/index.json 契约版本（ADR-0123 2026-11-14 复核）。
const DocIndexSchemaVersion = 1

// maxDocIndexBytes readmes/index.json 下载上限。
const maxDocIndexBytes = 32 << 20

// DocSource 文档源信息。
type DocSource struct {
	RepoURL         string `json:"repo_url"`
	Revision        string `json:"revision"`
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	SHA256          string `json:"sha256"`
	BaseURL         string `json:"base_url"`
	CatalogRevision string `json:"catalog_revision,omitempty"`
}

// DocLocale 单个语言的译文索引。
type DocLocale struct {
	Path             string `json:"path"`
	SHA256           string `json:"sha256"`
	Origin           string `json:"origin"`
	Review           string `json:"review"`
	SourceSHA256     string `json:"source_sha256"`
	GeneratorVersion string `json:"generator_version"`
}

// DocIndexEntry 单个扩展的文档索引项。
type DocIndexEntry struct {
	MarketplaceID  string               `json:"marketplace_id"`
	DocumentKey    string               `json:"document_key"`
	Source         DocSource            `json:"source"`
	Locales        map[string]DocLocale `json:"locales"`
	ProvenancePath string               `json:"provenance_path,omitempty"`
	RightsID       string               `json:"rights_id,omitempty"`
}

// DocIndex readmes/index.json 顶层结构。
type DocIndex struct {
	Schema      int                      `json:"schema"`
	GeneratedAt string                   `json:"generated_at"`
	Entries     map[string]DocIndexEntry `json:"entries"`
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// SanitizeDocIndex 校验契约版本并清洗索引条目：验证 64 位 document_key 与 catalog_id 一致性、
// 过滤非法路径遍历，转换为数据库行。缺少必需哈希或校验不通过时拒收并报错。
func SanitizeDocIndex(idx *DocIndex) ([]types.ExtDocIndexRow, error) {
	if idx == nil || idx.Schema != DocIndexSchemaVersion {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: invalid or unsupported doc index schema")
	}
	rows := make([]types.ExtDocIndexRow, 0, len(idx.Entries))
	for catID, e := range idx.Entries {
		row, err := sanitizeIndexEntry(catID, e)
		if err != nil {
			return nil, err
		}
		rows = append(rows, *row)
	}
	return rows, nil
}

func sanitizeIndexEntry(catID string, e DocIndexEntry) (*types.ExtDocIndexRow, error) {
	catID = strings.TrimSpace(catID)
	if catID == "" || strings.Contains(catID, "..") {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: invalid catalog_id in doc index")
	}
	expectedKey := DocumentKey(catID)
	if e.DocumentKey == "" || !strings.EqualFold(e.DocumentKey, expectedKey) {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: invalid or missing document_key for "+catID)
	}
	if !isHex64(e.Source.SHA256) {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: source sha256 must be valid 64-char hex for "+catID)
	}
	sourceRev := strings.TrimSpace(e.Source.Revision)
	if sourceRev == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: source revision missing for "+catID)
	}
	sourcePath := cleanDocPath(e.Source.Path)
	if sourcePath == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "marketplace: source path missing or invalid for "+catID)
	}

	sourceKind := strings.TrimSpace(e.Source.Kind)
	if sourceKind == "" {
		sourceKind = "readme"
	}
	row := &types.ExtDocIndexRow{
		CatalogID:       catID,
		MarketplaceID:   strings.TrimSpace(e.MarketplaceID),
		DocumentKey:     expectedKey,
		SourceRepoURL:   strings.TrimSpace(e.Source.RepoURL),
		SourceRevision:  sourceRev,
		SourcePath:      sourcePath,
		SourceKind:      sourceKind,
		SourceSHA256:    strings.TrimSpace(e.Source.SHA256),
		SourceBaseURL:   strings.TrimSpace(e.Source.BaseURL),
		CatalogRevision: strings.TrimSpace(e.Source.CatalogRevision),
		ProvenancePath:  cleanDocPath(e.ProvenancePath),
		RightsID:        strings.TrimSpace(e.RightsID),
	}

	if zhLocale, ok := e.Locales["zh-CN"]; ok {
		if err := populateZhLocale(catID, row.SourceSHA256, zhLocale, row); err != nil {
			return nil, err
		}
	}
	return row, nil
}

func populateZhLocale(catID, expectedSourceSHA string, zhLocale DocLocale, row *types.ExtDocIndexRow) error {
	zhPath := cleanDocPath(zhLocale.Path)
	if zhPath == "" {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: zh-CN path missing or invalid for "+catID)
	}
	if !isHex64(zhLocale.SHA256) {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: zh-CN sha256 must be valid 64-char hex for "+catID)
	}
	if !isHex64(zhLocale.SourceSHA256) || !strings.EqualFold(zhLocale.SourceSHA256, expectedSourceSHA) {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: zh-CN source_sha256 mismatch or invalid for "+catID)
	}
	origin := strings.TrimSpace(zhLocale.Origin)
	if origin != "upstream" && origin != "machine" && origin != "community" {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: zh-CN invalid origin for "+catID)
	}
	review := strings.TrimSpace(zhLocale.Review)
	if review != "reviewed" && review != "unreviewed" {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: zh-CN invalid review status for "+catID)
	}

	row.ZhPath = zhPath
	row.ZhSHA256 = strings.TrimSpace(zhLocale.SHA256)
	row.ZhOrigin = origin
	row.ZhReview = review
	row.ZhSourceSHA256 = strings.TrimSpace(zhLocale.SourceSHA256)
	row.GeneratorVersion = strings.TrimSpace(zhLocale.GeneratorVersion)
	return nil
}

// DocumentKey 计算 catalog_id 的 SHA-256 完整 64 位小写十六进制字符串（跨仓唯一关联键）。
func DocumentKey(catalogID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(catalogID)))
	return hex.EncodeToString(sum[:])
}

func cleanDocPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." || filepath.IsAbs(cleaned) {
		return ""
	}
	return cleaned
}

// DocIndexSync 集中说明文档索引同步器。
type DocIndexSync struct {
	repo protocol.ExtensionRepository
	http network.SafeHTTPClient
}

func NewDocIndexSync(repo protocol.ExtensionRepository, httpClient network.SafeHTTPClient) *DocIndexSync {
	return &DocIndexSync{repo: repo, http: httpClient}
}

// Sync 从远端拉取 readmes/index.json 并原子更新到数据库。
func (s *DocIndexSync) Sync(ctx context.Context, rawURL string) (int, error) {
	if s.repo == nil {
		return 0, apperr.New(apperr.CodeInternal, "marketplace: nil repo in DocIndexSync")
	}
	prevETag, err := s.repo.GetDocIndexMeta(ctx, "etag")
	if err != nil {
		prevETag = ""
	}
	body, etag, err := s.fetch(ctx, rawURL, prevETag)
	if err != nil {
		return 0, err
	}
	if body == nil {
		// 304 Not Modified
		return 0, nil
	}
	var idx DocIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "marketplace: parse doc index json", err)
	}
	rows, err := SanitizeDocIndex(&idx)
	if err != nil {
		return 0, err
	}
	meta := map[string]string{
		"etag":         etag,
		"generated_at": idx.GeneratedAt,
	}
	if err := s.repo.ReplaceDocIndex(ctx, rows, meta); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "marketplace: replace doc index", err)
	}
	return len(rows), nil
}

func (s *DocIndexSync) fetch(ctx context.Context, rawURL, prevETag string) ([]byte, string, error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return nil, "", apperr.New(apperr.CodeForbidden, "marketplace: doc index URL must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInvalidInput, "marketplace: build doc index request", err)
	}
	if prevETag != "" {
		req.Header.Set("If-None-Match", prevETag)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: doc index fetch", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, prevETag, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", apperr.New(apperr.CodeNetworkUnavailable, "marketplace: doc index returned "+resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocIndexBytes+1))
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: doc index read", err)
	}
	if len(body) > maxDocIndexBytes {
		return nil, "", apperr.New(apperr.CodeInvalidInput, "marketplace: doc index exceeds size limit")
	}
	return body, resp.Header.Get("ETag"), nil
}
