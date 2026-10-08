package tts

import (
	"strings"
	"unicode"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Model 是服务端 TTS 模型标识（ADR-0110）。
type Model string

const (
	// ModelMelo MeloTTS zh_en fp32（sherpa VITS 配置，44.1kHz，标准档）。
	ModelMelo Model = "melo"
	// ModelMatcha Matcha zh-en + vocos-16khz-univ（sherpa Matcha 配置，16kHz，轻量档）。
	ModelMatcha Model = "matcha"
)

// ParseModel 把名字解析为 Model；未知名字返回 CodeInvalidInput。
func ParseModel(s string) (Model, error) {
	switch Model(s) {
	case ModelMelo, ModelMatcha:
		return Model(s), nil
	}
	return "", apperr.New(apperr.CodeInvalidInput, "tts: 未知的 TTS 模型 \""+s+"\"（可选 melo | matcha）")
}

// requiredFiles 是各模型运行必需的文件（相对模型目录）；末尾带 "/" 的是目录。
// 缺任何一项引擎都会创建失败或读错音。
//
// Melo 的 model.int8.onnx 在归档里只是 133 字节占位文件，既不使用也不列为必需。
// 规则 FST 不在此列：缺失时只是数字/日期读法退化，由 NewEngine 留痕告警（沿用既有策略）。
func requiredFiles(m Model) []string {
	switch m {
	case ModelMelo:
		return []string{"model.onnx", "lexicon.txt", "tokens.txt", "dict/"}
	case ModelMatcha:
		return []string{"model-steps-3.onnx", MatchaVocoderFile, "lexicon.txt", "tokens.txt", "espeak-ng-data/"}
	}
	return nil
}

// MatchaVocoderFile 是 Matcha 声码器在模型目录里的文件名（单文件资产，与归档分开下载）。
const MatchaVocoderFile = "vocos-16khz-univ.onnx"

// ruleFstFiles 是各模型的文本规整 FST（电话/日期/数字等）。
// 缺失时数字、日期会按字面逐字读错，所以必须传给引擎。
func ruleFstFiles(m Model) []string {
	switch m {
	case ModelMelo:
		return []string{"phone.fst", "date.fst", "number.fst", "new_heteronym.fst"}
	case ModelMatcha:
		return []string{"phone-zh.fst", "date-zh.fst", "number-zh.fst"}
	}
	return nil
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
