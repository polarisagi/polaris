package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxInterpolatedStringBytes 插值后单个字符串的最大字节长度（64KiB）。
const maxInterpolatedStringBytes = 65536

// exactTemplateRegex 匹配整值模板替换，如 {{steps.merge.items}} 或 {{run.since}}。
var exactTemplateRegex = regexp.MustCompile(`^\{\{\s*([a-z_.0-9]+)\s*\}\}$`)

// RefResolver 引用解析器接口，为模板渲染提供运行时步骤数据与运行上下文。
type RefResolver interface {
	Step(key string) (StepData, bool)
	Run(name string) (string, bool)
}

// Render 递归遍历并渲染 args JSON 中的模板变量。
// 规则：
// 1. 若字符串整体匹配 `^{{...}}$`，按整值替换（如 steps.x.items 替换为真实 JSON 数组，steps.x.count 替换为数字）；
// 2. 否则按字符串插值（对象/数组转换为紧凑 JSON），单个插值结果限制 ≤ 64KiB；
// 3. 若引用了任何 steps.* 或 run.* 变量，refsUsed 置为 true；若引用未定义变量返回 CodeInvalidInput 错误。
func Render(args json.RawMessage, r RefResolver) (json.RawMessage, bool, error) {
	if len(args) == 0 || string(args) == "null" || string(args) == "{}" {
		return args, false, nil
	}

	var root any
	if err := json.Unmarshal(args, &root); err != nil {
		return nil, false, apperr.Wrap(apperr.CodeInvalidInput, "template: invalid json", err)
	}

	refsUsed := false
	rendered, used, err := renderValue(root, r)
	if err != nil {
		return nil, false, err
	}
	if used {
		refsUsed = true
	}

	out, err := json.Marshal(rendered)
	if err != nil {
		return nil, false, apperr.Wrap(apperr.CodeInternal, "template: marshal rendered json", err)
	}
	return out, refsUsed, nil
}

//nolint:gocyclo
func renderValue(v any, r RefResolver) (any, bool, error) {
	switch val := v.(type) {
	case string:
		// 1. 整值替换
		if m := exactTemplateRegex.FindStringSubmatch(val); len(m) > 1 {
			resolved, err := resolveExactRef(m[1], r)
			if err != nil {
				return nil, false, err
			}
			return resolved, true, nil
		}

		// 2. 字符串内局部插值
		if !strings.Contains(val, "{{") {
			return val, false, nil
		}

		used := false
		var replaceErr error
		res := templateVarRegex.ReplaceAllStringFunc(val, func(match string) string {
			if replaceErr != nil {
				return match
			}
			sub := templateVarRegex.FindStringSubmatch(match)
			if len(sub) <= 1 {
				return match
			}
			ref := sub[1]
			resolvedStr, err := resolveStringRef(ref, r)
			if err != nil {
				replaceErr = err
				return match
			}
			used = true
			return resolvedStr
		})

		if replaceErr != nil {
			return nil, false, replaceErr
		}
		if len(res) > maxInterpolatedStringBytes {
			return nil, false, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("template: interpolated string exceeds %d bytes (got %d)", maxInterpolatedStringBytes, len(res)))
		}
		return res, used, nil

	case []any:
		usedAny := false
		outList := make([]any, len(val))
		for i, item := range val {
			resItem, used, err := renderValue(item, r)
			if err != nil {
				return nil, false, err
			}
			if used {
				usedAny = true
			}
			outList[i] = resItem
		}
		return outList, usedAny, nil

	case map[string]any:
		usedAny := false
		outMap := make(map[string]any, len(val))
		for k, item := range val {
			resItem, used, err := renderValue(item, r)
			if err != nil {
				return nil, false, err
			}
			if used {
				usedAny = true
			}
			outMap[k] = resItem
		}
		return outMap, usedAny, nil

	default:
		return v, false, nil
	}
}

func resolveExactRef(ref string, r RefResolver) (any, error) {
	if r == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "template: ref resolver is nil")
	}

	if strings.HasPrefix(ref, "run.") {
		runKey := strings.TrimPrefix(ref, "run.")
		val, ok := r.Run(runKey)
		if !ok {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("template: unknown run ref %q", ref))
		}
		return val, nil
	}

	if strings.HasPrefix(ref, "steps.") {
		parts := strings.Split(ref, ".")
		if len(parts) != 3 {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("template: invalid step ref syntax %q", ref))
		}
		stepKey := parts[1]
		prop := parts[2]

		data, ok := r.Step(stepKey)
		if !ok {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("template: unknown step %q in ref %q", stepKey, ref))
		}

		switch prop {
		case "items":
			if data.Items == nil {
				return make([]map[string]any, 0), nil
			}
			return data.Items, nil
		case "count":
			return data.Count, nil
		case "text":
			return data.Text, nil
		default:
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("template: unknown step property %q in ref %q", prop, ref))
		}
	}

	return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("template: invalid reference %q", ref))
}

func resolveStringRef(ref string, r RefResolver) (string, error) {
	val, err := resolveExactRef(ref, r)
	if err != nil {
		return "", err
	}
	switch v := val.(type) {
	case string:
		return v, nil
	case int:
		return strconv.Itoa(v), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "template: marshal object to string", err)
		}
		return string(b), nil
	}
}
