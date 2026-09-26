package lifecycle

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// validateOptionValue 按 Claude userConfig 选项定义校验取值（类型 / options / multiple / min-max / required）。
func validateOptionValue(opt pluginspec.UserConfigOption, raw json.RawMessage) error {
	invalid := func(format string, args ...any) error {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config %q: ", opt.Key)+fmt.Sprintf(format, args...))
	}
	switch opt.Type {
	case "number":
		var n float64
		if json.Unmarshal(raw, &n) != nil {
			return invalid("must be a number")
		}
		if (opt.Min != nil && n < *opt.Min) || (opt.Max != nil && n > *opt.Max) {
			return invalid("out of range")
		}
		return nil
	case "boolean":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return invalid("must be a boolean")
		}
		return nil
	case "string", "directory", "file":
		return validateStringOption(opt, raw, invalid)
	}
	return invalid("unsupported option type %q", opt.Type)
}

func validateStringOption(opt pluginspec.UserConfigOption, raw json.RawMessage, invalid func(string, ...any) error) error {
	var values []string
	if opt.Multiple {
		if json.Unmarshal(raw, &values) != nil {
			return invalid("must be an array of strings")
		}
	} else {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return invalid("must be a string")
		}
		values = []string{one}
	}
	if opt.Required && (len(values) == 0 || strings.TrimSpace(values[0]) == "") {
		return invalid("is required")
	}
	if len(opt.Options) > 0 && !slices.Contains(opt.Options, values[0]) {
		return invalid("must be one of %v", opt.Options)
	}
	return nil
}
