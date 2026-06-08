package memory

import "strings"

// PlatformHintFor 返回指定平台的提示文本（不区分大小写）。
// 从 configs/prompts/platform/{platform}.md 加载（embedded，只读，Polaris 维护）。
// 未知平台返回空字符串。
func PlatformHintFor(platform string) string {
	key := strings.ToLower(strings.TrimSpace(platform))
	if key == "" {
		return ""
	}
	return ReadPrompt("platform/"+key+".md", "")
}
