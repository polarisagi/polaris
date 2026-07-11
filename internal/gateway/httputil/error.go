package httputil

import (
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// RespondError logs the detailed error internally and returns a sanitized HTTP error to the client.
func RespondError(w http.ResponseWriter, msg string, err error, code int) {
	if err != nil {
		slog.Warn("http request failed", "msg", msg, "error", apperr.Wrap(apperr.CodeInternal, msg, err))
	}
	http.Error(w, msg, code)
}
