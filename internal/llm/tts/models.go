package tts

import (
	"strings"
	"unicode"
)

// ModelName 是服务端 TTS 唯一模型 MeloTTS zh_en fp32 的标识（ADR-0110 修订：Matcha 已删除）。
// 用于状态接口的 model 字段、基准持久化键与指纹。
const ModelName = "melo"

// requiredFiles 是 MeloTTS 运行必需的文件（相对模型目录）；末尾带 "/" 的是目录。
// 缺任何一项引擎都会创建失败或读错音。
//
// 归档里的 model.int8.onnx 只是 133 字节占位文件，既不使用也不列为必需。
// 规则 FST 不在此列：缺失时只是数字/日期读法退化，由 NewEngine 留痕告警（沿用既有策略）。
func requiredFiles() []string {
	return []string{"model.onnx", "lexicon.txt", "tokens.txt", "dict/"}
}

// ruleFstFiles 是 MeloTTS 的文本规整 FST（电话/日期/数字/多音字）。
// 缺失时数字、日期会按字面逐字读错，所以必须传给引擎。
func ruleFstFiles() []string {
	return []string{"phone.fst", "date.fst", "number.fst", "new_heteronym.fst"}
}

// sentenceDelims 是切句的句末标点（ADR-0110 决策 6）。
// 刻意不含逗号：逗号处的拆分正是 sherpa 子句级拆批吞字的成因；也不含 ASCII "."，
// 避免小数、域名、版本号被切断。
const sentenceDelims = "。！？!?；;\n"

// closers 是紧跟句末标点的收尾符号，归入上一句（如 “好的。”、(完了!)）。
const closers = "”’\"')）】」』》"

// SplitSentences 按句末标点切句，标点留在句尾。
//
// 为什么服务端自己切：MeloTTS 必须 max_num_sentences=100（整句同批，=1 会吞字），
// 而整段长文本一次送入会让峰值内存随长度增长（20s 段 ≈810MB）。自己切句后逐句合成，
// 单次峰值只取决于最长一句。
// 不含任何字母/数字的碎片（如孤立的 "。"）并入上一句，不单独送去合成。
func SplitSentences(text string) []string {
	var out []string
	var pending string // 开头没有实义内容的碎片，并入第一句
	var buf strings.Builder

	flush := func() {
		seg := buf.String()
		buf.Reset()
		if strings.TrimSpace(seg) == "" {
			return
		}
		if !hasContent(seg) {
			if len(out) > 0 {
				out[len(out)-1] += seg
			} else {
				pending += seg
			}
			return
		}
		out = append(out, pending+seg)
		pending = ""
	}

	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		buf.WriteRune(r)
		if !strings.ContainsRune(sentenceDelims, r) {
			continue
		}
		// 连续的句末标点与收尾符号归入本句。
		for i+1 < len(rs) && (strings.ContainsRune(sentenceDelims, rs[i+1]) || strings.ContainsRune(closers, rs[i+1])) {
			i++
			buf.WriteRune(rs[i])
		}
		flush()
	}
	flush()
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func hasContent(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
