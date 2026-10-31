package httputil

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestRespondErrorPreservesCodeAndRetryAfterWithoutCause(t *testing.T) {
	cause := errors.New("secret-cause-sentinel")
	err := apperr.Wrap(apperr.CodeStorageUnavailable, "database unavailable", cause).WithRetryAfter(17)
	w := httptest.NewRecorder()

	RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "17" {
		t.Fatalf("Retry-After: want 17, got %q", got)
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != string(apperr.CodeStorageUnavailable) {
		t.Fatalf("code: want %s, got %s", apperr.CodeStorageUnavailable, body.Code)
	}
	if body.Message != http.StatusText(http.StatusServiceUnavailable) || body.Error != body.Message {
		t.Fatalf("unexpected public message fields: %+v", body)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type: %q", w.Header().Get("Content-Type"))
	}
	if w.Body.String() == "" || strings.Contains(w.Body.String(), cause.Error()) {
		t.Fatalf("cause leaked or body missing: %s", w.Body.String())
	}
}

func TestRespondErrorUsesSafeExplicitMessage(t *testing.T) {
	err := apperr.Wrap(apperr.CodeInternal, "operation failed", errors.New("private-cause-sentinel"))
	w := httptest.NewRecorder()

	RespondError(w, "private-cause-sentinel", err, http.StatusInternalServerError)

	if strings.Contains(w.Body.String(), "private-cause-sentinel") {
		t.Fatalf("cause leaked into response: %s", w.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Message != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("message: want safe status text, got %q", body.Message)
	}
}

func TestRespondErrorPreservesExplicitStatusAndOriginalCode(t *testing.T) {
	err := apperr.Wrap(apperr.CodeForbidden, "request denied", errors.New("private-cause-sentinel"))
	w := httptest.NewRecorder()

	// The status argument is the legacy transport contract; the body retains the
	// typed application code. Callers must choose a semantically matching pair.
	RespondError(w, "", err, http.StatusInternalServerError)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("explicit status was changed: got %d", w.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != string(apperr.CodeForbidden) {
		t.Fatalf("original application code was not preserved: %q", body.Code)
	}
	if body.Message != http.StatusText(http.StatusInternalServerError) || strings.Contains(w.Body.String(), "private-cause-sentinel") {
		t.Fatalf("unsafe public error response: %s", w.Body.String())
	}
}

func TestRespondErrorClassifiesUntypedErrorsFromExplicitClientStatus(t *testing.T) {
	w := httptest.NewRecorder()
	RespondError(w, "invalid request body", errors.New("decoder syntax details"), http.StatusBadRequest)

	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadRequest || body.Code != string(apperr.CodeInvalidInput) {
		t.Fatalf("untyped 400 error was not classified by HTTP semantics: status=%d body=%+v", w.Code, body)
	}
	if body.Message != "invalid request body" || strings.Contains(w.Body.String(), "decoder syntax details") {
		t.Fatalf("unexpected public error: %s", w.Body.String())
	}
}

func TestRespondErrorMaps503ToServiceUnavailableWithoutTypedError(t *testing.T) {
	w := httptest.NewRecorder()
	RespondError(w, "", nil, http.StatusServiceUnavailable)

	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != string(apperr.CodeServiceUnavailable) {
		t.Fatalf("503 reverse mapping: want %s, got %s", apperr.CodeServiceUnavailable, body.Code)
	}
}

func TestRespondErrorClassifiesAdditionalClientStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			w := httptest.NewRecorder()
			RespondError(w, "Request is invalid", nil, status)
			var body errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != status || body.Code != string(apperr.CodeInvalidInput) {
				t.Fatalf("status %d classification: got status=%d body=%+v", status, w.Code, body)
			}
		})
	}

	w := httptest.NewRecorder()
	RespondError(w, "Request timed out", nil, http.StatusRequestTimeout)
	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusRequestTimeout || body.Code != string(apperr.CodeTimeout) {
		t.Fatalf("408 classification: got status=%d body=%+v", w.Code, body)
	}
}

func TestRespondErrorLogOmitsCauseText(t *testing.T) {
	old := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	err := apperr.Wrap(apperr.CodeInternal, "operation failed", errors.New("private-cause-sentinel"))
	RespondError(httptest.NewRecorder(), "", err, http.StatusInternalServerError)

	if strings.Contains(logs.String(), "private-cause-sentinel") {
		t.Fatalf("cause text leaked into gateway log stream: %s", logs.String())
	}
	for _, want := range []string{"INTERNAL", "status=500", "error_type=*apperr.Error"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("structured diagnostic missing %q: %s", want, logs.String())
		}
	}
}
