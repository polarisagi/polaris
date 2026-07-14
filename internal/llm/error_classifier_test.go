package llm

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name          string
		errMsg        string
		wantReason    FailReason
		wantStatus    int
		wantRetryable bool
		wantCompress  bool
		wantRotate    bool
		wantFallback  bool
		wantTierInt   int
	}{
		// ── nil ──────────────────────────────────────────────────────────────
		{
			name:        "nil error",
			errMsg:      "", // special-cased below
			wantReason:  ReasonUnknown,
			wantTierInt: 3,
		},

		// ── 网络层 / 超时 ──────────────────────────────────────────────────────
		{
			name:          "context deadline exceeded",
			errMsg:        "[INTERNAL] post https://api.anthropic.com/v1/messages: context deadline exceeded",
			wantReason:    ReasonTimeout,
			wantStatus:    0,
			wantRetryable: true,
			wantTierInt:   2,
		},
		{
			name:          "i/o timeout",
			errMsg:        "dial tcp 1.2.3.4:443: i/o timeout",
			wantReason:    ReasonTimeout,
			wantRetryable: true,
			wantTierInt:   2,
		},
		{
			name:          "connection refused",
			errMsg:        "dial tcp 127.0.0.1:11434: connect: connection refused",
			wantReason:    ReasonServerError,
			wantRetryable: true,
			wantTierInt:   1,
		},

		// ── Anthropic 格式 ────────────────────────────────────────────────────
		{
			name:          "anthropic rate limit",
			errMsg:        `[INTERNAL] anthropic: HTTP 429: {"type":"error","error":{"type":"rate_limit_error","message":"Rate limit reached for claude-3-5-sonnet"}}`,
			wantReason:    ReasonRateLimit,
			wantStatus:    429,
			wantRetryable: true,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "anthropic auth transient",
			errMsg:        `[INTERNAL] anthropic: HTTP 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantReason:    ReasonAuth,
			wantStatus:    401,
			wantRetryable: false,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "anthropic overloaded 529",
			errMsg:        `[INTERNAL] anthropic: HTTP 529: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantReason:    ReasonOverloaded,
			wantStatus:    529,
			wantRetryable: true,
			wantRotate:    false, // 不换 provider，只退避
			wantTierInt:   2,
		},
		{
			name:          "anthropic context overflow 400",
			errMsg:        `[INTERNAL] anthropic: HTTP 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 220000 tokens > 200000 maximum. Please reduce the length of the messages."}}`,
			wantReason:    ReasonContextOverflow,
			wantStatus:    400,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
		{
			name:          "anthropic thinking signature",
			errMsg:        `[INTERNAL] anthropic: HTTP 400: {"error":{"type":"invalid_request_error","message":"Extended thinking signature verification failed"}}`,
			wantReason:    ReasonThinkingSignature,
			wantStatus:    400,
			wantRetryable: true,
			wantTierInt:   3,
		},
		{
			name:          "anthropic long context tier",
			errMsg:        `[INTERNAL] anthropic: HTTP 400: {"error":{"message":"To use 32k context, enable the long context tier in your account settings."}}`,
			wantReason:    ReasonLongContextTier,
			wantStatus:    400,
			wantRetryable: false,
			wantFallback:  true,
			wantTierInt:   3,
		},
		{
			name:          "anthropic server error 500",
			errMsg:        `[INTERNAL] anthropic: HTTP 500: {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			wantReason:    ReasonServerError,
			wantStatus:    500,
			wantRetryable: true,
			wantTierInt:   1,
		},

		// ── OpenAI 兼容格式 ───────────────────────────────────────────────────
		{
			name:          "openai rate limit",
			errMsg:        `[INTERNAL] api error (status 429): {"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`,
			wantReason:    ReasonRateLimit,
			wantStatus:    429,
			wantRetryable: true,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "openai billing 402",
			errMsg:        `[INTERNAL] api error (status 402): {"error":{"message":"You exceeded your current quota, please check your plan and billing details."}}`,
			wantReason:    ReasonBilling,
			wantStatus:    402,
			wantRetryable: false,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "openai model not found",
			errMsg:        `[INTERNAL] api error (status 404): {"error":{"message":"The model gpt-5-turbo does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			wantReason:    ReasonModelNotFound,
			wantStatus:    404,
			wantRetryable: false,
			wantFallback:  true,
			wantTierInt:   3,
		},
		{
			name:          "openai context overflow 400",
			errMsg:        `[INTERNAL] api error (status 400): {"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 200000 tokens. Please reduce the length of the messages.","code":"context_length_exceeded"}}`,
			wantReason:    ReasonContextOverflow,
			wantStatus:    400,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
		{
			name:          "openai format error 400",
			errMsg:        `[INTERNAL] api error (status 400): {"error":{"message":"Invalid value for 'temperature': must be a float","type":"invalid_request_error"}}`,
			wantReason:    ReasonFormatError,
			wantStatus:    400,
			wantRetryable: false,
			wantTierInt:   4,
		},
		{
			name:          "openai payload too large 413",
			errMsg:        `[INTERNAL] api error (status 413): request entity too large`,
			wantReason:    ReasonPayloadTooLarge,
			wantStatus:    413,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
		{
			name:          "openai image too large 413",
			errMsg:        `[INTERNAL] api error (status 413): image size exceeds maximum allowed for vision requests`,
			wantReason:    ReasonImageTooLarge,
			wantStatus:    413,
			wantRetryable: true,
			wantCompress:  false,
			wantTierInt:   2,
		},

		// ── Google / resource_exhausted ───────────────────────────────────────
		{
			name:          "google resource_exhausted maps to rate_limit",
			errMsg:        `[INTERNAL] api error (status 429): {"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
			wantReason:    ReasonRateLimit,
			wantStatus:    429,
			wantRetryable: true,
			wantRotate:    true,
			wantTierInt:   1,
		},

		// ── Ollama / llama.cpp ────────────────────────────────────────────────
		{
			name:          "ollama prompt too long",
			errMsg:        `[INTERNAL] api error (status 400): {"error":"prompt is too long (65536 tokens), please reduce the length"}`,
			wantReason:    ReasonContextOverflow,
			wantStatus:    400,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
		{
			name:          "llama.cpp grammar regex conflict",
			errMsg:        `[INTERNAL] api error (status 400): grammar error: regex pattern conflict with GGUF grammar`,
			wantReason:    ReasonLlamaCppGrammar,
			wantStatus:    400,
			wantRetryable: true,
			wantTierInt:   2,
		},

		// ── 503 overloaded ────────────────────────────────────────────────────
		{
			name:          "503 service unavailable",
			errMsg:        `[INTERNAL] api error (status 503): {"error":{"message":"Service temporarily unavailable"}}`,
			wantReason:    ReasonOverloaded,
			wantStatus:    503,
			wantRetryable: true,
			wantRotate:    false,
			wantTierInt:   2,
		},

		// ── provider policy blocked ───────────────────────────────────────────
		{
			name:          "403 data policy blocked",
			errMsg:        `[INTERNAL] api error (status 403): {"error":{"message":"This request violates our data policies."}}`,
			wantReason:    ReasonProviderPolicyBlocked,
			wantStatus:    403,
			wantRetryable: false,
			wantTierInt:   4,
		},

		// ── auth permanent ────────────────────────────────────────────────────
		{
			name:          "401 refresh token expired",
			errMsg:        `[INTERNAL] anthropic: HTTP 401: refresh token expired, cannot refresh credentials`,
			wantReason:    ReasonAuthPermanent,
			wantStatus:    401,
			wantRetryable: false,
			wantTierInt:   4,
		},

		// ── 429 usage limit ambiguity ─────────────────────────────────────────
		{
			name:          "429 usage limit with retry hint → rate_limit",
			errMsg:        `[INTERNAL] api error (status 429): You've reached your usage limit. Please try again after the limit resets in 24h.`,
			wantReason:    ReasonRateLimit,
			wantStatus:    429,
			wantRetryable: true,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "429 usage limit no retry hint → billing",
			errMsg:        `[INTERNAL] api error (status 429): You've reached your usage limit. Please upgrade your plan.`,
			wantReason:    ReasonBilling,
			wantStatus:    429,
			wantRetryable: false,
			wantRotate:    true,
			wantTierInt:   1,
		},

		// ── Google 专项 ───────────────────────────────────────────────────────
		{
			name:          "google input token count exceeds limit",
			errMsg:        `[INTERNAL] google: HTTP 400: {"error":{"code":400,"message":"Input token count (65536) exceeds this model's limit (32768).","status":"INVALID_ARGUMENT"}}`,
			wantReason:    ReasonContextOverflow,
			wantStatus:    400,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
		{
			name:          "google context window exceeded",
			errMsg:        `[INTERNAL] google: HTTP 400: {"error":{"code":400,"message":"Request exceeds this model's context window of 32768 tokens.","status":"INVALID_ARGUMENT"}}`,
			wantReason:    ReasonContextOverflow,
			wantStatus:    400,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
		{
			name:          "google unauthenticated 401",
			errMsg:        `[INTERNAL] google: HTTP 401: {"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`,
			wantReason:    ReasonAuth,
			wantStatus:    401,
			wantRetryable: false,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "google model not found 404",
			errMsg:        `[INTERNAL] google: HTTP 404: {"error":{"code":404,"message":"models/gemini-invalid is not found for API version v1beta.","status":"NOT_FOUND"}}`,
			wantReason:    ReasonModelNotFound,
			wantStatus:    404,
			wantRetryable: false,
			wantFallback:  true,
			wantTierInt:   3,
		},

		// ── 中文 provider ─────────────────────────────────────────────────────
		{
			name:          "中文 provider 流量超限",
			errMsg:        `[INTERNAL] api error (status 429): 流量超限，请稍后重试`,
			wantReason:    ReasonRateLimit,
			wantStatus:    429,
			wantRetryable: true,
			wantRotate:    true,
			wantTierInt:   1,
		},
		{
			name:          "中文 provider 上下文超限",
			errMsg:        `[INTERNAL] api error (status 400): 超出最大 token 限制，请缩短输入`,
			wantReason:    ReasonContextOverflow,
			wantStatus:    400,
			wantRetryable: true,
			wantCompress:  true,
			wantTierInt:   2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ce ClassifiedError
			if tc.errMsg == "" {
				ce = Classify(nil)
			} else {
				ce = Classify(apperr.New(apperr.CodeInternal, tc.errMsg))
			}

			if ce.Reason != tc.wantReason {
				t.Errorf("Reason: got %q, want %q", ce.Reason, tc.wantReason)
			}
			if tc.wantStatus != 0 && ce.Status != tc.wantStatus {
				t.Errorf("Status: got %d, want %d", ce.Status, tc.wantStatus)
			}
			if ce.Retryable != tc.wantRetryable {
				t.Errorf("Retryable: got %v, want %v", ce.Retryable, tc.wantRetryable)
			}
			if ce.ShouldCompress != tc.wantCompress {
				t.Errorf("ShouldCompress: got %v, want %v", ce.ShouldCompress, tc.wantCompress)
			}
			if ce.ShouldRotateCredential != tc.wantRotate {
				t.Errorf("ShouldRotateCredential: got %v, want %v", ce.ShouldRotateCredential, tc.wantRotate)
			}
			if ce.ShouldFallback != tc.wantFallback {
				t.Errorf("ShouldFallback: got %v, want %v", ce.ShouldFallback, tc.wantFallback)
			}
			// 2026-07-14（ADR-0051）：ClassifiedError.FallbackTierInt 已删除（全仓零
			// 生产调用点），wantTierInt 断言随之移除；字段本身保留在表格用例中
			// 不产生编译错误（未使用的 struct 字段合法），故无需逐条清理测试数据。
		})
	}
}

func TestClassifyWithProvider(t *testing.T) {
	ce := ClassifyWithProvider(apperr.New(apperr.CodeInternal, "[INTERNAL] anthropic: HTTP 429: rate limit"), "anthropic")
	if ce.Provider != "anthropic" {
		t.Errorf("Provider: got %q, want %q", ce.Provider, "anthropic")
	}
	if ce.Reason != ReasonRateLimit {
		t.Errorf("Reason: got %q, want rate_limit", ce.Reason)
	}
}

func TestExtractHTTPStatus(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"[INTERNAL] anthropic: HTTP 429: body", 429},
		{"[INTERNAL] api error (status 503): body", 503},
		{"dial tcp: connection refused", 0},
		{"HTTP 200 OK", 200},
		{"status=401 unauthorized", 401},
	}
	for _, c := range cases {
		got := extractHTTPStatus(c.msg)
		if got != c.want {
			t.Errorf("extractHTTPStatus(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}
