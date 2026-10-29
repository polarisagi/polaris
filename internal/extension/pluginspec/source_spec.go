package pluginspec

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ParseSourceSpec 解析用户直接给出的插件/技能来源字符串（两家 CLI 的安装输入形式）：
//
//	owner/repo[@ref|#ref]                           GitHub 仓库
//	owner/repo/sub/dir[@ref|#ref]                   GitHub 仓库子目录（git-subdir）
//	https://github.com/o/r/tree/<ref>/<path>        GitHub 网页上的目录地址（Codex skill-installer 形式）
//	https://host/repo.git[#ref]、https://host/repo   任意 git 仓库（url）
//	https://host/plugin.zip                         zip 包（archive）
//	npm:<package>[@<version|range|tag>]             npm 包
//	/abs/local/dir                                  本机目录（仅管理员自建宿主）
//
// 只接受 https 远程地址；ssh / file 协议拒绝（git 子进程绕过 SafeDialer，见 downloader.GitFetchRevision）。
func ParseSourceSpec(spec string) (PluginSource, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "":
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "source is required")
	case strings.HasPrefix(spec, "npm:"):
		return npmSpec(strings.TrimPrefix(spec, "npm:"))
	case filepath.IsAbs(spec):
		if strings.Contains(spec, "..") {
			return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "local path must not contain \"..\"")
		}
		return PluginSource{Type: SourceRelative, Path: filepath.Clean(spec)}, nil
	case strings.HasPrefix(spec, "https://"):
		return httpsSpec(spec)
	case strings.Contains(spec, "://") || strings.HasPrefix(spec, "git@"):
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "only https sources are supported")
	}
	return githubShorthand(spec)
}

func npmSpec(s string) (PluginSource, error) {
	pkg, version := s, ""
	// 作用域包以 @ 开头，版本分隔符是其后的第二个 @。
	if at := strings.LastIndex(s, "@"); at > 0 {
		pkg, version = s[:at], s[at+1:]
	}
	if pkg == "" || strings.Contains(pkg, "..") {
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "invalid npm package")
	}
	return PluginSource{Type: SourceNPM, Package: pkg, Version: version}, nil
}

func httpsSpec(spec string) (PluginSource, error) {
	u, err := url.Parse(spec)
	if err != nil || u.Host == "" || u.User != nil {
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "invalid https source")
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".zip") {
		return PluginSource{Type: SourceArchive, URL: spec}, nil
	}
	ref := u.Fragment
	u.Fragment = ""
	if u.Host == "github.com" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && parts[2] == "tree" {
			repo := "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git") + ".git"
			if len(parts) == 4 {
				return PluginSource{Type: SourceURL, URL: repo, Ref: parts[3]}, nil
			}
			return PluginSource{Type: SourceGitSubdir, URL: repo, Ref: parts[3], Path: strings.Join(parts[4:], "/")}, nil
		}
	}
	return PluginSource{Type: SourceURL, URL: u.String(), Ref: ref}, nil
}

func githubShorthand(spec string) (PluginSource, error) {
	ref := ""
	if i := strings.LastIndexAny(spec, "@#"); i > 0 {
		spec, ref = spec[:i], spec[i+1:]
	}
	parts := strings.Split(strings.Trim(spec, "/"), "/")
	if len(parts) < 2 || !githubRepoPattern.MatchString(parts[0]+"/"+parts[1]) {
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "source must be owner/repo, an https URL, npm:<package> or an absolute path")
	}
	repo := parts[0] + "/" + parts[1]
	if len(parts) == 2 {
		return PluginSource{Type: SourceGitHub, Repo: repo, Ref: ref}, nil
	}
	sub := strings.Join(parts[2:], "/")
	if strings.Contains(sub, "..") {
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "path must not contain \"..\"")
	}
	return PluginSource{Type: SourceGitSubdir, URL: "https://github.com/" + repo + ".git", Path: sub, Ref: ref}, nil
}
