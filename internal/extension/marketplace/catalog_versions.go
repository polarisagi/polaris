package marketplace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// resolveVersion 按依赖版本范围选择来源版本（Claude plugin dependencies「How a constraint resolves
// against tags」）：
//   - github / url / git-subdir：插件自有仓库的 "<name>--v<version>" 标签，无满足范围的标签即失败；
//   - 相对路径：市场仓库的同名标签；无满足范围的标签时用市场当前副本，由加载期检查兜底；
//   - npm：范围交给注册表解析；archive：只在加载期检查。
func (c *CatalogInstaller) resolveVersion(ctx context.Context, marketplaceID string, entry pluginspec.MarketplaceEntry, constraint string) (pluginspec.PluginSource, string, error) {
	src := entry.Source
	if constraint == "" {
		return src, "", nil
	}
	cons, err := semver.NewConstraint(constraint)
	if err != nil {
		return src, "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("dependency %s: invalid version range %q", entry.Name, constraint))
	}
	switch src.Type {
	case pluginspec.SourceNPM:
		src.Version = constraint
		return src, "", nil
	case pluginspec.SourceGitHub, pluginspec.SourceURL, pluginspec.SourceGitSubdir:
		tag, err := c.highestTag(ctx, sourceRepo(src), entry.Name, cons)
		if err != nil {
			return src, "", err
		}
		if tag == "" {
			return src, "", apperr.New(apperr.CodeNotFound, fmt.Sprintf("Dependency %q has no git tag satisfying %s", entry.Name, constraint))
		}
		return src, tag, nil
	case pluginspec.SourceRelative:
		return c.relativeAtTag(ctx, marketplaceID, entry, cons)
	}
	return src, "", nil
}

func (c *CatalogInstaller) relativeAtTag(ctx context.Context, marketplaceID string, entry pluginspec.MarketplaceEntry, cons *semver.Constraints) (pluginspec.PluginSource, string, error) {
	mp, err := c.extRepo.GetMarketplace(ctx, marketplaceID)
	if err != nil || mp == nil || !strings.HasPrefix(mp.RepoURL, "https://") || strings.HasSuffix(mp.RepoURL, ".json") {
		return entry.Source, "", nil //nolint:nilerr // 非 git 市场没有标签，按当前副本安装
	}
	tag, err := c.highestTag(ctx, mp.RepoURL, entry.Name, cons)
	if err != nil || tag == "" {
		return entry.Source, "", nil //nolint:nilerr // 无满足范围的标签：用当前副本，加载期检查兜底（Claude 规则）
	}
	rel, err := filepath.Rel(c.sync.MarketplaceDir(marketplaceID), entry.Source.Path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return entry.Source, "", nil //nolint:nilerr
	}
	return pluginspec.PluginSource{Type: pluginspec.SourceGitSubdir, URL: mp.RepoURL, Path: filepath.ToSlash(rel)}, tag, nil
}

func sourceRepo(src pluginspec.PluginSource) string {
	if src.Type == pluginspec.SourceGitHub {
		return "https://github.com/" + src.Repo + ".git"
	}
	return gitURL(src.URL)
}

// highestTag 在 "<name>--v<version>" 标签中取满足范围的最高版本；没有返回空串。
func (c *CatalogInstaller) highestTag(ctx context.Context, repoURL, name string, cons *semver.Constraints) (string, error) {
	tags, err := downloader.GitListTags(ctx, func(u string) error { return network.ValidateGitURL(ctx, u) }, repoURL)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "resolve tags "+repoURL, err)
	}
	return pickTag(tags, name, cons), nil
}

func pickTag(tags []string, name string, cons *semver.Constraints) string {
	var best *semver.Version
	bestTag := ""
	for _, t := range tags {
		raw, ok := strings.CutPrefix(t, name+"--v")
		if !ok {
			continue
		}
		v, err := semver.NewVersion(raw)
		if err == nil && cons.Check(v) && (best == nil || v.GreaterThan(best)) {
			best, bestTag = v, t
		}
	}
	return bestTag
}
