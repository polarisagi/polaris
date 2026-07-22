package httputil

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// RespondError logs the detailed error internally and returns a sanitized HTTP error to the client.
func RespondError(w http.ResponseWriter, msg string, err error, code int) {
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
