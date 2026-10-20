package policy

import "strings"

func (t *cmdTable) addGit() {
	t.set(gitHandler, "git")
}

// gitSubcommand 跳过全局选项，返回子命令与其参数；遇 -c / --git-dir 等可注入执行的全局选项标记为危险来源。
func (c *cc) gitSubcommand() (sub string, rest []string, risky bool) {
	args := c.args
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			if c.pathsState([]string{args[i+1]}) != pathInside {
				risky = true
			}
			i++
		case a == "-c" || strings.HasPrefix(a, "--git-dir") || strings.HasPrefix(a, "--work-tree") || strings.HasPrefix(a, "--exec-path"):
			risky = true
			if a == "-c" {
				i++
			}
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:], risky
		}
	}
	return "", nil, risky
}

func gitHandler(c *cc) verdict {
	sub, rest, risky := c.gitSubcommand()
	if risky {
		return unknownV("git_global_option")
	}
	if v, ok := gitDangerous(sub, rest); ok {
		return v
	}
	if gitReadOnly(sub, rest) {
		return safeV()
	}
	v := unknownV("git_subcommand")
	v.network = gitNetworkSubs()[sub]
	return v
}

func gitNetworkSubs() map[string]bool {
	return map[string]bool{"push": true, "pull": true, "fetch": true, "clone": true, "ls-remote": true}
}

// gitDangerous 改写历史/丢弃改动/强推等不可逆操作。
func gitDangerous(sub string, rest []string) (verdict, bool) {
	switch sub {
	case "push":
		if hasShort(rest, 'f') || hasLong(rest, "--force", "--force-with-lease", "--delete", "--mirror", "--prune") || hasPlusRefspec(rest) || hasShort(rest, 'd') {
			v := dangerV("git_force_push")
			v.network = true
			return v, true
		}
	case "reset":
		if hasLong(rest, "--hard") {
			return dangerV("git_reset_hard"), true
		}
	case "clean":
		return dangerV("git_clean"), true
	case "filter-branch", "filter-repo", "update-ref", "gc", "prune":
		return dangerV("git_rewrite_history"), true
	case "reflog":
		if s, _ := firstNonFlag(rest); s == "expire" || s == "delete" {
			return dangerV("git_rewrite_history"), true
		}
	}
	return verdict{}, false
}

func hasPlusRefspec(args []string) bool {
	for _, a := range positionals(args) {
		if strings.HasPrefix(a, "+") {
			return true
		}
	}
	return false
}

func gitReadOnlySubs() map[string]bool {
	return map[string]bool{
		"status": true, "diff": true, "log": true, "show": true, "blame": true, "rev-parse": true, "ls-files": true,
		"ls-tree": true, "cat-file": true, "describe": true, "shortlog": true, "rev-list": true, "grep": true,
		"show-ref": true, "for-each-ref": true, "merge-base": true, "name-rev": true, "whatchanged": true, "diff-tree": true,
	}
}

func gitReadOnly(sub string, rest []string) bool {
	if gitReadOnlySubs()[sub] {
		return true
	}
	switch sub {
	case "branch":
		return gitBranchReadOnly(rest)
	case "tag":
		return len(positionals(rest)) == 0 && !hasShort(rest, 'd') && !hasShort(rest, 'a') && !hasShort(rest, 's') && !hasShort(rest, 'f')
	case "config":
		return hasLong(rest, "--get", "--get-all", "--get-regexp", "--list") || hasShort(rest, 'l')
	}
	return gitSubActionReadOnly(sub, rest)
}

// gitSubActionReadOnly 处理"git <sub> <action>"形态：只读动作白名单，其余一律非只读。
func gitSubActionReadOnly(sub string, rest []string) bool {
	s, _ := firstNonFlag(rest)
	switch sub {
	case "remote":
		return s == "" || s == "show" || s == "get-url"
	case "stash":
		return s == "list" || s == "show"
	case "worktree", "submodule":
		return s == "list" || s == "status"
	}
	return false
}

func gitBranchReadOnly(rest []string) bool {
	for _, f := range []string{"-d", "-D", "-m", "-M", "-c", "-C", "-u", "--delete", "--move", "--copy",
		"--set-upstream-to", "--unset-upstream", "--edit-description", "--force"} {
		if hasLong(rest, f) {
			return false
		}
	}
	for _, c := range []byte{'d', 'D', 'm', 'M', 'c', 'C', 'u', 'f'} {
		if hasShort(rest, c) {
			return false
		}
	}
	listFlag := hasShort(rest, 'l') || hasShort(rest, 'a') || hasShort(rest, 'r') || hasShort(rest, 'v') ||
		hasLong(rest, "--list", "--contains", "--merged", "--no-merged", "--show-current", "--all", "--remotes")
	return len(positionals(rest)) == 0 || listFlag
}
