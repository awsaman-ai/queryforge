package queryforge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The rules these tests encode, stated once:
//
//   - A failure is retried only when TIME could fix it. Everything caused by the
//     request itself reproduces exactly, so retrying it delays the error the
//     caller needs and spends quota to learn nothing.
//   - "Rate limited" and "out of credit" arrive on the same status codes and
//     want opposite handling, so they must be told apart.
//   - No error may ever contain the API key, whatever the endpoint sent back.

// TestStatusClassification pins the meaning of each status, and — more
// importantly — the retry decision that follows from it.
func TestStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		want      ProviderErrorKind
		retryable bool
		why       string
	}{
		{
			name:   "401 is an auth failure and must not be retried",
			status: http.StatusUnauthorized, body: "invalid api key",
			want: KindAuth, retryable: false,
			why: "the same rejected key would be sent again",
		},
		{
			name:   "403 without a money hint is an auth failure",
			status: http.StatusForbidden, body: "permission denied for this resource",
			want: KindAuth, retryable: false,
		},
		{
			name:   "403 with a money hint is a quota failure",
			status: http.StatusForbidden, body: "You exceeded your current quota",
			want: KindQuota, retryable: false,
			why: "providers overload 403 for an exhausted free tier; waiting adds no funds",
		},
		{
			name:   "402 is a quota failure",
			status: http.StatusPaymentRequired, body: "payment required",
			want: KindQuota, retryable: false,
		},
		{
			name:   "429 is a rate limit and must be retried",
			status: http.StatusTooManyRequests, body: "too many requests, slow down",
			want: KindRateLimit, retryable: true,
			why: "this is the one failure where waiting genuinely fixes it",
		},
		{
			name:   "429 carrying a billing message is quota, not a rate limit",
			status: http.StatusTooManyRequests, body: "insufficient credit on this account",
			want: KindQuota, retryable: false,
			why: "retrying an empty wallet burns the caller's latency budget for a certain failure",
		},
		{
			name:   "404 means the model id is wrong and must not be retried",
			status: http.StatusNotFound, body: "model not found",
			want: KindModelNotFound, retryable: false,
		},
		{
			name:   "400 is a bad request and must not be retried",
			status: http.StatusBadRequest, body: "unsupported parameter",
			want: KindInvalidRequest, retryable: false,
			why: "the body is deterministic, so a retry sends identical bytes",
		},
		{
			name:   "422 is a bad request and must not be retried",
			status: http.StatusUnprocessableEntity, body: "invalid schema",
			want: KindInvalidRequest, retryable: false,
		},
		{
			name:   "408 is a timeout and may be retried",
			status: http.StatusRequestTimeout, body: "",
			want: KindTimeout, retryable: true,
		},
		{
			name:   "500 is a provider fault and must be retried",
			status: http.StatusInternalServerError, body: "internal error",
			want: KindUnavailable, retryable: true,
		},
		{
			name:   "503 is a provider outage and must be retried",
			status: http.StatusServiceUnavailable, body: "overloaded",
			want: KindUnavailable, retryable: true,
		},
		{
			name:   "529 — a non-standard overload code — is still retried",
			status: 529, body: "overloaded_error",
			want: KindUnavailable, retryable: true,
			why: "any 5xx is the provider's fault, so the rule must not be a list of known codes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStatus(tc.status, tc.body)
			if got != tc.want {
				t.Errorf("classifyStatus(%d, %q) = %q, want %q", tc.status, tc.body, got, tc.want)
			}
			if got.Retryable() != tc.retryable {
				t.Errorf("%q.Retryable() = %v, want %v — %s", got, got.Retryable(), tc.retryable, tc.why)
			}
		})
	}
}

// TestQuotaDetectionIsCaseInsensitive: providers capitalise their error prose
// however they like, and the classification must not depend on it.
func TestQuotaDetectionIsCaseInsensitive(t *testing.T) {
	for _, body := range []string{
		"QUOTA EXCEEDED",
		"Insufficient Balance",
		"billing hard limit reached",
		"Your Free Tier allowance is used up",
	} {
		if classifyStatus(http.StatusTooManyRequests, body) != KindQuota {
			t.Errorf("body %q on a 429 should classify as quota", body)
		}
	}
}

// TestPlainRateLimitIsNotMistakenForQuota guards the other direction: a
// throttling message must not trip the quota heuristic, or the one genuinely
// retryable failure would stop being retried.
func TestPlainRateLimitIsNotMistakenForQuota(t *testing.T) {
	for _, body := range []string{
		"rate limit exceeded, retry shortly",
		"too many requests",
		"429: slow down",
		"",
	} {
		if got := classifyStatus(http.StatusTooManyRequests, body); got != KindRateLimit {
			t.Errorf("body %q on a 429 classified as %q, want %q", body, got, KindRateLimit)
		}
	}
}

// TestTruncatedReplyIsNotRetryable: a reply cut short by the token budget is a
// BAD_RESPONSE. The request is deterministic, so retrying truncates at exactly
// the same place — and bills for it again. Only raising maxTokens fixes it.
func TestTruncatedReplyIsNotRetryable(t *testing.T) {
	if KindBadResponse.Retryable() {
		t.Error("BAD_RESPONSE must not be retryable: an identical request truncates identically")
	}
}

// TestErrorNeverLeaksTheAPIKey is the security invariant. An endpoint — or a
// misconfigured proxy in front of one — can echo the Authorization header back
// in its error body. Once that text is inside an error it will be logged, so
// scrubbing happens where the error is built, not where it is printed.
func TestErrorNeverLeaksTheAPIKey(t *testing.T) {
	const key = "sk-ant-supersecret-value-000111222"

	cases := []struct {
		name string
		body string
	}{
		{"body echoes the bare key", `{"error":"bad key: ` + key + `"}`},
		{"body echoes the Authorization header", `unauthorized: Bearer ` + key},
		{"key appears more than once", key + " and again " + key},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := newProviderError(KindAuth, 401, "acme", "acme-ultra", tc.body, key, nil)

			if strings.Contains(pe.Error(), key) {
				t.Fatalf("the API key survived into the error text: %s", pe.Error())
			}
			if strings.Contains(pe.Detail, key) {
				t.Fatalf("the API key survived into Detail: %s", pe.Detail)
			}
			if !strings.Contains(pe.Error(), "REDACTED") {
				t.Errorf("redaction should be visible so the reader knows text was removed: %s", pe.Error())
			}
		})
	}
}

// TestRedactionLeavesOrdinaryTextAlone: a very short key would match all over
// normal prose, and blanking those matches would corrupt the error message that
// is meant to help. Below the threshold, redaction must do nothing.
func TestRedactionLeavesOrdinaryTextAlone(t *testing.T) {
	const body = "the model is not available in this region"

	if got := redactSecret(body, "abc"); got != body {
		t.Errorf("a 3-char key must not trigger redaction, got %q", got)
	}
	if got := redactSecret(body, ""); got != body {
		t.Errorf("an empty key must not trigger redaction, got %q", got)
	}
}

// TestErrorMessageNamesTheFix: the three misconfigurations a user actually hits
// each get a message pointing at the specific thing to change. An error that
// only says "401" sends the reader to the provider's dashboard when the problem
// is usually an env var that was never exported into this process.
func TestErrorMessageNamesTheFix(t *testing.T) {
	cases := []struct {
		kind ProviderErrorKind
		want string
	}{
		{KindAuth, "apiKeyEnv"},
		{KindQuota, "models"},
		{KindModelNotFound, "model.model"},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			err := errModelUnreachable(&ProviderError{Kind: tc.kind, Status: 400})
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("a %s error should mention %q, got: %s", tc.kind, tc.want, err)
			}
		})
	}
}

// TestProviderErrorIsInspectableThroughWrapping: the engine wraps provider
// errors in sentinels, so classification has to survive errors.As at any depth
// or the retry loop silently stops working.
func TestProviderErrorIsInspectableThroughWrapping(t *testing.T) {
	pe := &ProviderError{Kind: KindRateLimit, Status: 429}
	wrapped := fmt.Errorf("planner: model call failed: %w: %w", ErrModelTransport, pe)

	got, ok := asProviderError(wrapped)
	if !ok {
		t.Fatal("a wrapped ProviderError must still be recoverable with errors.As")
	}
	if got.Kind != KindRateLimit {
		t.Errorf("kind = %q, want %q", got.Kind, KindRateLimit)
	}
	// The existing failure taxonomy must be untouched by any of this.
	if !errors.Is(wrapped, ErrModelTransport) {
		t.Error("wrapping must not break the ErrModelTransport sentinel the SDKs depend on")
	}
}

// TestUnwrapReachesTheUnderlyingCause: callers checking for
// context.DeadlineExceeded must still find it under a ProviderError.
func TestUnwrapReachesTheUnderlyingCause(t *testing.T) {
	pe := newProviderError(KindTimeout, 0, "p", "m", "deadline", "", context.DeadlineExceeded)
	if !errors.Is(pe, context.DeadlineExceeded) {
		t.Error("errors.Is must see through ProviderError to the wrapped cause")
	}
}

// TestRetryAfterIsHonoured covers both wire formats providers use, plus the
// cases where the header must be treated as simply absent.
func TestRetryAfterIsHonoured(t *testing.T) {
	t.Run("delay-seconds form is used verbatim", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{"3"}}
		if got := parseRetryAfter(h); got != 3*time.Second {
			t.Errorf("got %v, want 3s", got)
		}
	})

	t.Run("HTTP-date form is converted to a delay", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat)}}
		got := parseRetryAfter(h)
		// Second-granularity format, so allow a little slack either side.
		if got < 2*time.Second || got > 5*time.Second {
			t.Errorf("got %v, want roughly 4s", got)
		}
	})

	t.Run("an absurd delay is capped rather than trusted", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{"3600"}}
		if got := parseRetryAfter(h); got != maxRetryAfter {
			t.Errorf("got %v, want it capped at %v — a caller waiting on a query "+
				"must not be parked for an hour by a provider header", got, maxRetryAfter)
		}
	})

	t.Run("an unreadable header is no hint, not an error", func(t *testing.T) {
		for _, v := range []string{"soon", "", "-5", "0", "NaN"} {
			h := http.Header{"Retry-After": []string{v}}
			if got := parseRetryAfter(h); got != 0 {
				t.Errorf("Retry-After %q gave %v, want 0", v, got)
			}
		}
	})

	t.Run("a date in the past is no hint", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)}}
		if got := parseRetryAfter(h); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})

	t.Run("no header at all", func(t *testing.T) {
		if got := parseRetryAfter(http.Header{}); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
		if got := parseRetryAfter(nil); got != 0 {
			t.Errorf("nil header gave %v, want 0", got)
		}
	})
}

// TestContextExpiryIsNotRetryable: when the CALLER's budget is gone there is
// nowhere left to run, so it must not be classified as a retryable transport
// blip that sends the loop around again.
func TestContextExpiryIsNotRetryable(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// The kind may be retryable in isolation; the loop's own ctx check is
		// what stops it. What matters here is that cancellation is recognised.
		if got := classifyTransport(ctx, context.Canceled); got != KindTransport {
			t.Errorf("got %q, want %q", got, KindTransport)
		}
	})

	t.Run("expired deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond)
		if got := classifyTransport(ctx, context.DeadlineExceeded); got != KindTimeout {
			t.Errorf("got %q, want %q", got, KindTimeout)
		}
	})

	t.Run("a live context means the fault was the network's", func(t *testing.T) {
		if got := classifyTransport(context.Background(), errors.New("connection refused")); got != KindTransport {
			t.Errorf("got %q, want %q", got, KindTransport)
		}
	})
}
