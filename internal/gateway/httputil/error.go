package httputil

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func defaultMessageForStatus(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "Bad Request"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusNotFound:
		return "Not Found"
	case http.StatusUnauthorized:
		return "Unauthorized"
	default:
		return "Internal Server Error"
	}
}

// RespondError logs the detailed error internally and returns a sanitized HTTP error to the client.
func RespondError(w http.ResponseWriter, msg string, err error, code int) {
	// 当 message 为 "Internal Server Error" 且 HTTP status 不是 500 时，
	// 自动修正为与 status code 匹配的语义化消息，避免误导用户（GR-9-003）
	if msg == "" || (msg == "Internal Server Error" && code != http.StatusInternalServerError) {
		msg = defaultMessageForStatus(code)
	}

	if err != nil {
		slog.Warn("http request failed", "msg", msg, "error", apperr.Wrap(apperr.CodeInternal, msg, err))
		// 资源耗尽/限流类错误若携带建议重试间隔，透传为 Retry-After 响应头。
		var ae *apperr.Error
		if errors.As(err, &ae) && ae.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(ae.RetryAfter))
		}
	}
	http.Error(w, msg, code)
}
