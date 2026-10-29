package marketplace

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const defaultNPMRegistry = "https://registry.npmjs.org"

type npmPackument struct {
	DistTags map[string]string     `json:"dist-tags"`
	Versions map[string]npmVersion `json:"versions"`
}

type npmVersion struct {
	Dist struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// fetchNPM 从 npm 注册表取回包并解包（不执行任何生命周期脚本，不安装依赖——两家一致）。
// version 接受精确版本、dist-tag 或 semver 范围；registry 须为无凭据、无查询/片段的 https 地址。
func (f *SourceFetcher) fetchNPM(ctx context.Context, src pluginspec.PluginSource, destDir string) (Fetched, error) {
	registry, err := npmRegistry(src.Registry)
	if err != nil {
		return Fetched{}, err
	}
	doc, err := f.packument(ctx, registry, src.Package)
	if err != nil {
		return Fetched{}, err
	}
	version, err := pickNPMVersion(doc, src.Version)
	if err != nil {
		return Fetched{}, apperr.Wrap(apperr.CodeNotFound, "marketplace: npm "+src.Package, err)
	}
	dist := doc.Versions[version].Dist
	tgz, _, err := f.download(ctx, dist.Tarball, nil, "npm-*.tgz")
	if err != nil {
		return Fetched{}, err
	}
	defer os.Remove(tgz) //nolint:errcheck
	if err := verifyIntegrity(tgz, dist.Integrity); err != nil {
		return Fetched{}, err
	}
	in, err := os.Open(tgz)
	if err != nil {
		return Fetched{}, apperr.Wrap(apperr.CodeInternal, "marketplace: open tarball", err)
	}
	defer in.Close()
	// npm 包内容位于 package/ 前缀下；只提取普通文件（符号链接等一律跳过）。
	mapper := func(name string) (string, bool) {
		rel, ok := strings.CutPrefix(filepath.ToSlash(name), "package/")
		if !ok || rel == "" {
			return "", false
		}
		return filepath.Join(destDir, filepath.FromSlash(rel)), true
	}
	if err := downloader.ExtractTarGz(in, destDir, mapper); err != nil {
		return Fetched{}, apperr.Wrap(apperr.CodeInvalidInput, "marketplace: extract npm package", err)
	}
	return Fetched{Dir: destDir, Version: version}, nil
}

func npmRegistry(raw string) (string, error) {
	if raw == "" {
		return defaultNPMRegistry, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", apperr.New(apperr.CodeInvalidInput, "marketplace: npm registry must be an https URL without credentials, query or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (f *SourceFetcher) packument(ctx context.Context, registry, pkg string) (*npmPackument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/"+url.PathEscape(pkg), nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "marketplace: npm request", err)
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: npm metadata "+pkg, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeNotFound, "marketplace: npm package "+pkg+": "+resp.Status)
	}
	var doc npmPackument
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&doc); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "marketplace: npm metadata "+pkg, err)
	}
	return &doc, nil
}

// pickNPMVersion 空 = latest 标签；dist-tag 名；精确版本；否则取满足范围的最高版本。
func pickNPMVersion(doc *npmPackument, want string) (string, error) {
	if want == "" {
		want = "latest"
	}
	if v, ok := doc.DistTags[want]; ok {
		return v, nil
	}
	if _, ok := doc.Versions[want]; ok {
		return want, nil
	}
	c, err := semver.NewConstraint(want)
	if err != nil {
		return "", apperr.New(apperr.CodeInvalidInput, "invalid version selector "+want)
	}
	var best *semver.Version
	for raw := range doc.Versions {
		v, err := semver.NewVersion(raw)
		if err == nil && c.Check(v) && (best == nil || v.GreaterThan(best)) {
			best = v
		}
	}
	if best == nil {
		return "", apperr.New(apperr.CodeNotFound, "no version satisfies "+want)
	}
	return best.Original(), nil
}

// verifyIntegrity 校验 npm dist.integrity（sha512 SRI）；注册表未提供时拒绝——无法确认下载内容。
func verifyIntegrity(path, integrity string) error {
	b64, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: npm package has no sha512 integrity")
	}
	want, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "marketplace: npm integrity", err)
	}
	fh, err := os.Open(path)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marketplace: open tarball", err)
	}
	defer fh.Close()
	h := sha512.New()
	if _, err := io.Copy(h, fh); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marketplace: hash tarball", err)
	}
	if string(h.Sum(nil)) != string(want) {
		return apperr.New(apperr.CodeInvalidInput, "marketplace: npm package integrity mismatch")
	}
	return nil
}
