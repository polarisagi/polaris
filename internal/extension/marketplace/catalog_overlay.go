package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 目录覆盖层（ADR-0123）：中文名称/摘要/说明 + 四维分类，由官方插件仓库定时生成为单个
// index.json，守护进程整份拉取、校验后替换 extension_catalog_overlay。覆盖层只影响展示，
// 不参与安装决策，故失败时保留旧数据、不影响市场同步本身。

// OverlaySchemaVersion index.json 契约版本；生成器与守护进程必须一致，不一致整份拒收。
const OverlaySchemaVersion = 1

// maxOverlayBytes index.json 下载上限：3 千条目约 2MB，留足增长余量又防异常文件耗内存。
const maxOverlayBytes = 32 << 20

// 覆盖层字段长度上限（按字符计）：超长截断而非拒收，避免一条坏数据废掉整份覆盖层。
const (
	overlayMaxNameRunes    = 60
	overlayMaxSummaryRunes = 120
	overlayMaxDescRunes    = 1200
	overlayMaxIDRunes      = 300
)

// OverlayFacets 四个分类维度的固定键名；生成器与前端按此读取（函数而非包级变量：R1.3）。
func OverlayFacets() []string { return []string{"topic", "department", "industry", "life"} }

// overlayFacetMax 每个维度每条目最多保留的取值数（与 taxonomy.yaml facets.*.max 一致）。
func overlayFacetMax(facet string) int {
	if facet == "topic" {
		return 3
	}
	return 2
}

// OverlayLabel 分类取值的中英文显示名。
type OverlayLabel struct {
	En string `json:"en"`
	Zh string `json:"zh"`
}

// OverlayTaxonomy 随 index.json 下发的词表：facet → 取值 ID → 显示名。
type OverlayTaxonomy struct {
	Version int                                `json:"version"`
	Values  map[string]map[string]OverlayLabel `json:"values"`
}

// OverlayZh 条目的中文文本。
type OverlayZh struct {
	Name        string `json:"name,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
}

// OverlayEntry index.json 中单个目录条目的覆盖数据。
type OverlayEntry struct {
	// H 上游 name+description 的指纹（OverlaySourceHash），用于判断中文是否过期。
	H  string    `json:"h"`
	Zh OverlayZh `json:"zh"`
	// EnDescription 上游目录条目描述为空时，生成器从插件清单补出的英文描述。
	EnDescription string   `json:"en_description,omitempty"`
	Topic         []string `json:"topic,omitempty"`
	Department    []string `json:"department,omitempty"`
	Industry      []string `json:"industry,omitempty"`
	Life          []string `json:"life,omitempty"`
	Featured      int      `json:"featured,omitempty"`
}

// OverlayIndex index.json 顶层结构。
type OverlayIndex struct {
	Schema      int                     `json:"schema"`
	GeneratedAt string                  `json:"generated_at"`
	Taxonomy    OverlayTaxonomy         `json:"taxonomy"`
	Entries     map[string]OverlayEntry `json:"entries"`
}

// OverlaySourceHash 上游文本指纹：生成器写入 index.json，网关读目录时用同一函数比对。
// 两端必须共用此函数——任何一端自行实现都会让「是否过期」判断悄悄失真。
func OverlaySourceHash(name, description string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(name) + "\n" + strings.TrimSpace(description)))
	return hex.EncodeToString(sum[:])[:12]
}

// SanitizeOverlay 校验契约版本并清洗全部条目：截断超长文本、去控制字符、丢弃词表外的分类取值。
// 返回可直接落库的行；词表以 JSON 原样返回供落库。
func SanitizeOverlay(idx *OverlayIndex) ([]types.ExtCatalogOverlayRow, string, error) {
	if idx == nil || idx.Schema != OverlaySchemaVersion {
		return nil, "", apperr.New(apperr.CodeInvalidInput, "marketplace: unsupported overlay schema")
	}
	known := map[string]map[string]bool{}
	cleanTax := OverlayTaxonomy{Version: idx.Taxonomy.Version, Values: map[string]map[string]OverlayLabel{}}
	for _, f := range OverlayFacets() {
		known[f] = map[string]bool{}
		cleanTax.Values[f] = map[string]OverlayLabel{}
		for id, l := range idx.Taxonomy.Values[f] {
			id = cleanText(id, 64)
			if id == "" {
				continue
			}
			known[f][id] = true
			cleanTax.Values[f][id] = OverlayLabel{En: cleanText(l.En, overlayMaxNameRunes), Zh: cleanText(l.Zh, overlayMaxNameRunes)}
		}
	}
	taxJSON, err := json.Marshal(cleanTax)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInternal, "marketplace: marshal overlay taxonomy", err)
	}
	rows := make([]types.ExtCatalogOverlayRow, 0, len(idx.Entries))
	for id, e := range idx.Entries {
		id = strings.TrimSpace(id)
		if id == "" || utf8.RuneCountInString(id) > overlayMaxIDRunes || e.H == "" {
			continue
		}
		rows = append(rows, types.ExtCatalogOverlayRow{
			CatalogID:     id,
			SrcHash:       cleanText(e.H, 64),
			NameZh:        cleanText(e.Zh.Name, overlayMaxNameRunes),
			SummaryZh:     cleanText(e.Zh.Summary, overlayMaxSummaryRunes),
			DescriptionZh: cleanText(e.Zh.Description, overlayMaxDescRunes),
			DescriptionEn: cleanText(e.EnDescription, overlayMaxDescRunes),
			Topics:        facetJSON(e.Topic, known["topic"], overlayFacetMax("topic")),
			Departments:   facetJSON(e.Department, known["department"], overlayFacetMax("department")),
			Industries:    facetJSON(e.Industry, known["industry"], overlayFacetMax("industry")),
			Life:          facetJSON(e.Life, known["life"], overlayFacetMax("life")),
			Featured:      e.Featured,
		})
	}
	return rows, string(taxJSON), nil
}

// cleanText 去掉控制字符（保留换行）、折叠首尾空白并按字符截断。
func cleanText(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes])
}

// facetJSON 只保留词表内取值、去重、截到上限，序列化为 JSON 数组字符串。
func facetJSON(vals []string, known map[string]bool, maxN int) string {
	out := make([]string, 0, maxN)
	seen := map[string]bool{}
	for _, v := range vals {
		if len(out) >= maxN {
			break
		}
		if known[v] && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	b, _ := json.Marshal(out) //nolint:errcheck // []string 序列化不会失败
	return string(b)
}

// 覆盖层元数据键（extension_catalog_overlay_meta.key）。
const (
	OverlayMetaETag        = "etag"
	OverlayMetaGeneratedAt = "generated_at"
	OverlayMetaTaxonomy    = "taxonomy"
)

// OverlaySync 拉取并落库目录覆盖层。
type OverlaySync struct {
	extRepo protocol.ExtensionRepository
	http    network.SafeHTTPClient
	url     string
}

// NewOverlaySync url 为 index.json 的 https 地址（config.DefaultCatalogOverlayURL 或环境变量覆盖）。
func NewOverlaySync(extRepo protocol.ExtensionRepository, httpClient network.SafeHTTPClient, url string) *OverlaySync {
	return &OverlaySync{extRepo: extRepo, http: httpClient, url: url}
}

// Sync 条件请求 index.json：304 返回 (0, nil)；200 则校验、清洗后整份替换。返回落库条目数。
func (o *OverlaySync) Sync(ctx context.Context) (int, error) {
	if o == nil || o.extRepo == nil || o.url == "" {
		return 0, apperr.New(apperr.CodeUnimplemented, "marketplace: overlay sync not configured")
	}
	if !strings.HasPrefix(o.url, "https://") {
		return 0, apperr.New(apperr.CodeForbidden, "marketplace: overlay url must be https")
	}
	body, etag, err := o.fetch(ctx)
	if err != nil || body == nil {
		return 0, err // body == nil 且无错误 = 304 未变化
	}
	var idx OverlayIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "marketplace: overlay decode", err)
	}
	rows, taxJSON, err := SanitizeOverlay(&idx)
	if err != nil {
		return 0, err
	}
	meta := map[string]string{
		OverlayMetaETag:        etag,
		OverlayMetaGeneratedAt: cleanText(idx.GeneratedAt, 40),
		OverlayMetaTaxonomy:    taxJSON,
	}
	if err := o.extRepo.ReplaceCatalogOverlay(ctx, rows, meta); err != nil {
		return 0, apperr.Wrap(apperr.CodeOf(err), "marketplace: overlay store", err)
	}
	return len(rows), nil
}

// fetch 条件 GET index.json。304 返回 (nil, "", nil)；200 返回正文与新 ETag。
func (o *OverlaySync) fetch(ctx context.Context) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url, nil)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInvalidInput, "marketplace: overlay request", err)
	}
	if etag, err := o.extRepo.GetCatalogOverlayMeta(ctx, OverlayMetaETag); err == nil && etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: overlay fetch", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", apperr.New(apperr.CodeNetworkUnavailable, "marketplace: overlay returned "+resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOverlayBytes+1))
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: overlay read", err)
	}
	if len(body) > maxOverlayBytes {
		return nil, "", apperr.New(apperr.CodeInvalidInput, "marketplace: overlay exceeds size limit")
	}
	return body, resp.Header.Get("ETag"), nil
}

// Entries 取回单个市场的目录条目而不落库（目录覆盖层生成器 tools/catalog_overlay_gen.go 用）。
// 发布方与信任档位按 Sync 的同一规则填充，保证生成器看到的条目 ID 与守护进程一致。
func (s *CatalogSync) Entries(ctx context.Context, mp protocol.Marketplace) ([]protocol.RegistryEntry, error) {
	entries, err := s.entries(ctx, mp, false)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Publisher = firstNonEmpty(entries[i].Publisher, mp.Publisher)
		entries[i].TrustTier = mp.TrustTier
	}
	return entries, nil
}
