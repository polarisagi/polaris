package policy

import "strings"

// 已知的环境初始化生成器（eval "$(X)" 惯用法）：输出由工具自己生成、参数为字面量，
// 不是运行期拼出的任意代码，原文扫描不因 eval 的运行期展开升灾难性（仍按 eval 的原有级别处理）。
// "<shell>" 匹配常见 shell 名；尾部允许额外的 -- 选项（如 fnm env --use-on-cd）。
func evalGenerators() [][]string {
	out := make([][]string, 0, 32)
	out = append(out, [][]string{
		{"ssh-agent", "-s"}, {"ssh-agent", "-c"},
		{"direnv", "hook", "<shell>"},
		{"pyenv", "init", "-"}, {"pyenv", "init", "--path"}, {"pyenv", "virtualenv-init", "-"},
		{"rbenv", "init", "-"}, {"nodenv", "init", "-"},
		{"conda", "shell.<shell>", "hook"},
		{"starship", "init", "<shell>"}, {"zoxide", "init", "<shell>"},
		{"fnm", "env"},
		{"mise", "activate", "<shell>"},
	}...)
	for _, b := range []string{"brew", "/opt/homebrew/bin/brew", "/usr/local/bin/brew", "/home/linuxbrew/.linuxbrew/bin/brew"} {
		out = append(out, []string{b, "shellenv"}, []string{b, "shellenv", "<shell>"})
	}
	return out
}

func isShellName(s string) bool {
	switch s {
	case "bash", "zsh", "fish", "sh", "ksh", "dash", "tcsh", "csh", "powershell", "pwsh", "nu", "elvish", "xonsh":
		return true
	}
	return false
}

// isEvalGenerator words（字面量，无运行期展开）是否命中白名单。
func isEvalGenerator(words []string) bool {
	for _, pat := range evalGenerators() {
		if len(words) < len(pat) {
			continue
		}
		ok := true
		for i, p := range pat {
			switch {
			case p == "<shell>":
				ok = isShellName(words[i])
			case strings.Contains(p, "<shell>"):
				ok = strings.HasPrefix(words[i], strings.TrimSuffix(p, "<shell>")) && isShellName(strings.TrimPrefix(words[i], strings.TrimSuffix(p, "<shell>")))
			default:
				ok = words[i] == p
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		rest := true
		for _, extra := range words[len(pat):] {
			if !strings.HasPrefix(extra, "--") {
				rest = false
			}
		}
		if rest {
			return true
		}
	}
	return false
}
