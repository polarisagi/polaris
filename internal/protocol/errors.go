package protocol

import "github.com/polarisagi/polaris/pkg/apperr"

// ErrAllProvidersFailed 所有 Provider 耗尽哨兵
var ErrAllProvidersFailed = apperr.New(apperr.CodeInternal, "inference: all providers exhausted")
