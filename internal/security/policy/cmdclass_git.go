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
	// 破坏性判定先于全局选项判定：git -c k=v reset --hard 不能借 -c 降级成"未知"
	if v, ok := gitDangerous(sub, rest); ok {
		return v
	}
	if risky {
		return unknownV("git_global_option")
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
// 丢弃工作树未提交改动、stash、强推受保护分支不可恢复，归灾难性（ADR-0122 决策三）；其余保持危险。
func gitDangerous(sub string, rest []string) (verdict, bool) {
	if sub == "push" {
		return gitPush(rest)
	}
	if v, ok := gitDiscard(sub, rest); ok {
		return v, true
	}
	switch sub {
	case "branch":
		if hasShort(rest, 'D') {
			return dangerV("git_branch_delete"), true
		}
	case "filter-branch", "filter-repo", "update-ref", "gc", "prune":
		return dangerV("git_rewrite_history"), true
	case "reflog":
		if s, _ := firstNonFlag(rest); s == "expire" || s == "delete" {
			return dangerV("git_rewrite_history"), true
		}
	}
	return verdict{}, false
}

// gitDiscard 丢弃工作树/暂存区未提交改动的子命令。
func gitDiscard(sub string, rest []string) (verdict, bool) {
	switch sub {
	case "reset":
		if hasLong(rest, "--hard") {
			return catastropheV("git_discard_worktree"), true
		}
	case "clean":
		return gitClean(rest), true
	case "checkout":
		if gitCheckoutDiscardsAll(rest) {
			return catastropheV("git_discard_worktree"), true
		}
	case "restore":
		if !gitRestoreStagedOnly(rest) && !gitInteractive(rest) && pathspecIsWholeTree(rest) {
			return catastropheV("git_discard_worktree"), true
		}
	case "stash":
		if s, _ := firstNonFlag(rest); s == "drop" || s == "clear" {
			return catastropheV("git_stash_discard"), true
		}
	}
	return verdict{}, false
}

// gitCheckoutDiscardsAll checkout 带 "." 路径规格（整树从索引/提交覆盖）；-b/-B/--orphan 是建分支，-p 是交互挑选。
func gitCheckoutDiscardsAll(rest []string) bool {
	if hasShort(rest, 'b') || hasShort(rest, 'B') || hasLong(rest, "--orphan") || gitInteractive(rest) {
		return false
	}
	return pathspecIsWholeTree(rest)
}

func gitInteractive(rest []string) bool { return hasShort(rest, 'p') || hasLong(rest, "--patch") }

// gitRestoreStagedOnly restore 只动暂存区（--staged 且未同时要求 --worktree），不丢工作树改动。
func gitRestoreStagedOnly(rest []string) bool {
	return (hasLong(rest, "--staged") || hasShort(rest, 'S')) && !hasLong(rest, "--worktree") && !hasShort(rest, 'W')
}

// gitClean -n 只预览（安全）；-f 真删未跟踪文件（灾难性）；其余保持危险。
func gitClean(rest []string) verdict {
	switch {
	case hasShort(rest, 'n') || hasLong(rest, "--dry-run"):
		return safeV()
	case hasShort(rest, 'f') || hasLong(rest, "--force"):
		return catastropheV("git_discard_worktree")
	}
	return dangerV("git_clean")
}

// pathspecIsWholeTree 位置参数里出现 "."（整个工作树）。
func pathspecIsWholeTree(rest []string) bool {
	for _, p := range positionals(rest) {
		if p == "." || p == "./" || p == ":/" {
			return true
		}
	}
	return false
}

// gitPush 强推判定：对 main/master/trunk/release* 或未写明分支的强推灾难性，其余强推/删除保持危险。
func gitPush(rest []string) (verdict, bool) {
	force := hasShort(rest, 'f') || hasLong(rest, "--force", "--force-with-lease", "--force-if-includes", "--mirror") || hasPlusRefspec(rest)
	if !force && !hasLong(rest, "--delete", "--prune") && !hasShort(rest, 'd') {
		return verdict{}, false
	}
	v := dangerV("git_force_push")
	if force && pushTargetsProtected(rest) {
		v = catastropheV("git_force_push_protected")
	}
	v.network = true
	return v, true
}

// pushTargetsProtected 目标分支未写明（含 HEAD、--all、--mirror）或属于受保护分支。
func pushTargetsProtected(rest []string) bool {
	if hasLong(rest, "--all", "--mirror") {
		return true
	}
	pos := positionals(rest)
	if len(pos) < 2 {
		return true // 只写了远端或什么都没写：靠 push.default 推当前分支，无法确认
	}
	for _, ref := range pos[1:] {
		ref = strings.TrimPrefix(ref, "+")
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			ref = ref[i+1:]
		}
		ref = strings.TrimPrefix(ref, "refs/heads/")
		switch {
		case ref == "" || ref == "HEAD":
			return true
		case ref == "main" || ref == "master" || ref == "trunk" || strings.HasPrefix(ref, "release"):
			return true
		}
	}
	return false
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
