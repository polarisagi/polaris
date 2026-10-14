package web_fetch

// paginate 按 rune 字符切分文本，支持 offset 与 maxChars 分页。
func paginate(s string, offset, maxChars int) (chunk string, nextOffset int, truncated bool) {
	runes := []rune(s)
	total := len(runes)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return "", 0, false
	}
	if maxChars <= 0 {
		maxChars = total
	}
	end := offset + maxChars
	if end >= total {
		return string(runes[offset:]), 0, false
	}
	return string(runes[offset:end]), end, true
}
