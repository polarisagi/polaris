package tool

import (
	"math"
	"strings"
	"unicode"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
)

// BM25 参数取通用默认值；语料只有几十个工具，不值得调参。
const (
	bm25K1       = 1.5
	bm25B        = 0.75
	nameBoostRep = 3 // 名称词在文档里重复的次数，等价于加权
)

// searchTokens 把文本切成检索词（小写）：
//   - 拉丁字母/数字连续串为一个词，下划线、连字符、标点都是分隔符（"memory_search" → memory, search）；
//   - 汉字逐字成词，再加相邻汉字二元组（"定时任务" → 定,时,任,务,定时,时任,任务），
//     不依赖分词词典也能命中"定时"这类词。
func searchTokens(s string) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	var prevHan rune
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			out = append(out, string(r))
			if prevHan != 0 {
				out = append(out, string([]rune{prevHan, r}))
			}
			prevHan = r
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			prevHan = 0
			word = append(word, r)
		default:
			prevHan = 0
			flush()
		}
	}
	flush()
	return out
}

// bm25Scores 返回 query 对每个候选的 BM25 分（与 all 同序）。文档 = 名称词×nameBoostRep + 描述词。
func bm25Scores(all []protocol.CatalogEntry, query string) []float64 {
	n := len(all)
	scores := make([]float64, n)
	if n == 0 {
		return scores
	}
	docs := make([]map[string]int, n)
	lens := make([]int, n)
	df := map[string]int{}
	var total int
	for i, e := range all {
		tf := map[string]int{}
		for _, t := range searchTokens(e.Name) {
			tf[t] += nameBoostRep
			lens[i] += nameBoostRep
		}
		for _, t := range searchTokens(e.Description) {
			tf[t]++
			lens[i]++
		}
		// 检索别名（中英双语关键词 + 类别）按描述同权计入。
		for _, t := range searchTokens(catalog.SearchAliases(e.Name)) {
			tf[t]++
			lens[i]++
		}
		docs[i] = tf
		total += lens[i]
		for t := range tf {
			df[t]++
		}
	}
	avg := float64(total) / float64(n)
	if avg == 0 {
		return scores
	}
	seen := map[string]bool{}
	for _, q := range searchTokens(query) {
		if seen[q] {
			continue
		}
		seen[q] = true
		d := df[q]
		if d == 0 {
			continue
		}
		idf := math.Log(1 + (float64(n)-float64(d)+0.5)/(float64(d)+0.5))
		for i := range all {
			f := float64(docs[i][q])
			if f == 0 {
				continue
			}
			scores[i] += idf * f * (bm25K1 + 1) / (f + bm25K1*(1-bm25B+bm25B*float64(lens[i])/avg))
		}
	}
	return scores
}
