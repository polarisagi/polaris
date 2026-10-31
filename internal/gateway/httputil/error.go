package httputil

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func codeForHTTPStatus(status int) apperr.Code {
	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return apperr.CodeInvalidInput
	case http.StatusUnauthorized:
		return apperr.CodeUnauthorized
	case http.StatusForbidden:
		return apperr.CodeForbidden
	case http.StatusNotFound:
		return apperr.CodeNotFound
	case http.StatusConflict:
		return apperr.CodeConflict
	case http.StatusTooManyRequests:
		return apperr.CodeResourceExhausted
	case http.StatusNotImplemented:
		return apperr.CodeUnimplemented
	case http.StatusBadGateway:
		return apperr.CodeNetworkUnavailable
	case http.StatusServiceUnavailable:
		return apperr.CodeServiceUnavailable
	case http.StatusGatewayTimeout:
		return apperr.CodeTimeout
	case http.StatusRequestTimeout:
		return apperr.CodeTimeout
	default:
		return apperr.CodeInternal
	}
}

func containsErrorText(message string, err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		text := current.Error()
		if text != "" && strings.Contains(message, text) {
			return true
		}
	}
	return false
}

// PublicErrorMessage 返回适合 HTTP/SSE 客户端的状态摘要，不接受或展开底层错误文本。
func PublicErrorMessage(status int) string {
	if message := http.StatusText(status); message != "" {
		return message
	}
	return http.StatusText(http.StatusInternalServerError)
}

// RespondError 保留调用方传入的 HTTP status 以兼容既有路由契约，并在有错误时
// 保留其 apperr code。调用方负责让两者语义一致；该出口不会静默改写显式 status。
// 响应只包含公开消息，不把 Cause 发给调用方。
func RespondError(w http.ResponseWriter, msg string, err error, status int) {
	code := codeForHTTPStatus(status)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			code = ae.Code
			if ae.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(ae.RetryAfter))
			}
		}
		slog.Warn("http request failed", "code", string(code), "status", status,
			"error_type", fmt.Sprintf("%T", err))
	}

	if msg == "" || containsErrorText(msg, err) {
		msg = PublicErrorMessage(status)
	}
	WriteJSONStatus(w, status, errorResponse{Code: string(code), Message: msg, Error: msg})
}
