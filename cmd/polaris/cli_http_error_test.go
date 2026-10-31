package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCLIHTTPErrorText(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		want string
	}{
		{name: "public message first", code: 400, body: `{"code":"invalid","message":"bad input","error":"detail"}`, want: "HTTP 400: bad input"},
		{name: "string error", code: 403, body: `{"error":"denied"}`, want: "HTTP 403: denied"},
		{name: "OpenAI nested error", code: 429, body: `{"error":{"message":"quota exceeded"}}`, want: "HTTP 429: quota exceeded"},
		{name: "legacy text", code: 502, body: "upstream unavailable", want: "HTTP 502: upstream unavailable"},
		{name: "empty body", code: 503, body: "", want: "HTTP 503: request failed"},
		{name: "malformed JSON", code: 400, body: `{"message":`, want: "HTTP 400: request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			rr.WriteHeader(tt.code)
			_, _ = io.WriteString(rr, tt.body)
			resp := rr.Result()
			defer resp.Body.Close()
			if got := cliHTTPErrorText(resp); got != tt.want {
				t.Fatalf("cliHTTPErrorText() = %q, want %q", got, tt.want)
			}
		})
	}
}

type trackedErrorBody struct {
	data         *strings.Reader
	readAfterEOF int
	eof          bool
}

func (b *trackedErrorBody) Read(p []byte) (int, error) {
	if b.eof {
		b.readAfterEOF++
		return 0, io.EOF
	}
	n, err := b.data.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

func (b *trackedErrorBody) Close() error { return nil }

func TestCLIHTTPErrorTextConsumesBodyOnce(t *testing.T) {
	body := &trackedErrorBody{data: strings.NewReader(`{"message":"bad input"}`)}
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: body}
	if got := cliHTTPErrorText(resp); got != "HTTP 400: bad input" {
		t.Fatalf("cliHTTPErrorText() = %q", got)
	}
	if body.readAfterEOF != 0 {
		t.Fatalf("response body was read again after EOF: %d reads", body.readAfterEOF)
	}
}
