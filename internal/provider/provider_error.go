package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/awsaman-ai/queryforge/internal/failure"
)

// ProviderError is a model-call failure with its cause classified. It is what
// every built-in provider returns, so the retry loop can decide without parsing
// error strings.
//
// It carries no credential. See newProviderError: the response snippet is
// redacted before it is ever stored.
type ProviderError struct {
	Kind     failure.ProviderErrorKind // why it failed, and whether time might fix it
	Status   int                       // HTTP status, or 0 when the call never got one
	Provider string                    // provider label, for the message
	Model    string                    // model id, for the message
	Detail   string                    // redacted, bounded description
	Err      error                     // wrapped cause, if any

	// RetryAfter is the provider's own instruction on how long to wait, parsed
	// from the Retry-After header. Zero when absent. Honoured in preference to
	// computed backoff — a provider that tells you when to come back knows
	// better than an exponential curve does.
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	var b strings.Builder
	b.WriteString("provider")
	if e.Provider != "" {
		b.WriteString(" ")
		b.WriteString(e.Provider)
	}
	if e.Model != "" {
		b.WriteString(" (")
		b.WriteString(e.Model)
		b.WriteString(")")
	}
	b.WriteString(": ")
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		b.WriteString(" [HTTP ")
		b.WriteString(strconv.Itoa(e.Status))
		b.WriteString("]")
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Unwrap exposes the underlying cause so errors.Is/As still reach it — notably
// context.DeadlineExceeded and context.Canceled.
func (e *ProviderError) Unwrap() error { return e.Err }

// Retryable reports whether this specific failure should be retried.
func (e *ProviderError) Retryable() bool { return e.Kind.Retryable() }

// classifyStatus maps an HTTP status onto a kind.
//
// Status codes are mapped, not response text: every provider words its errors
// differently and rewords them without notice, so matching on prose would be a
// permanent maintenance tax that silently degrades. The two exceptions are 403
// and 429, where the status alone is genuinely ambiguous — see below.
func classifyStatus(status int, body string) failure.ProviderErrorKind {
	switch status {
	case http.StatusUnauthorized: // 401
		return failure.KindAuth

	case http.StatusForbidden: // 403
		// Ambiguous in practice: some providers use 403 for a revoked key and
		// others for an exhausted free tier. A quota hint decides it, because
		// the two want opposite handling — fail over vs. fail fast to the user.
		if looksLikeQuota(body) {
			return failure.KindQuota
		}
		return failure.KindAuth

	case http.StatusPaymentRequired: // 402
		return failure.KindQuota

	case http.StatusNotFound: // 404
		return failure.KindModelNotFound

	case http.StatusRequestTimeout: // 408
		return failure.KindTimeout

	case http.StatusTooManyRequests: // 429
		// A 429 usually means "slow down" (retry) but several providers reuse
		// it for "you are out of credit" (do not retry — no amount of waiting
		// adds funds). Only a quota hint distinguishes them.
		if looksLikeQuota(body) {
			return failure.KindQuota
		}
		return failure.KindRateLimit
	}

	switch {
	case status >= 500:
		return failure.KindUnavailable
	case status >= 400:
		// 400, 413, 422 and friends: the endpoint understood us and said no.
		return failure.KindInvalidRequest
	default:
		return failure.KindBadResponse
	}
}

// quotaHints are the phrases providers use when the problem is money or a spent
// allowance rather than pacing. Matched case-insensitively, and only to
// disambiguate 403/429 — never as the primary classification signal.
var quotaHints = []string{
	"quota",
	"insufficient",
	"credit",
	"billing",
	"payment",
	"exceeded your current",
	"out of funds",
	"free tier",
	"plan limit",
}

func looksLikeQuota(body string) bool {
	low := strings.ToLower(body)
	for _, h := range quotaHints {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

// classifyTransport maps a failure that never produced a status onto a kind.
func classifyTransport(ctx context.Context, err error) failure.ProviderErrorKind {
	// The parent context expiring is not the provider's fault and is not
	// retryable: the caller's whole budget is gone, so a further attempt has
	// nowhere to run. A per-attempt deadline expiring IS retryable, and that
	// case is distinguished by the caller's context still being live.
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return failure.KindTransport
		}
		return failure.KindTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure.KindTimeout
	}
	if errors.Is(err, context.Canceled) {
		return failure.KindTransport
	}
	return failure.KindTransport
}

// newProviderError builds a classified error, redacting the credential from the
// detail text before it is stored.
//
// Redaction happens HERE, at construction, rather than at logging time. An error
// travels further than any logger: into a caller's own logs, an exception
// message, a bug report, a crash dump. Scrubbing at the single point where the
// text is created means there is no path by which the raw key can escape,
// including paths this library does not control.
func newProviderError(kind failure.ProviderErrorKind, status int, providerID, model, detail, apiKey string, err error) *ProviderError {
	return &ProviderError{
		Kind:     kind,
		Status:   status,
		Provider: providerID,
		Model:    model,
		Detail:   redactSecret(detail, apiKey),
		Err:      err,
	}
}

// redactSecret removes the API key from text destined for an error message.
//
// A provider's error body should never contain the caller's key, but "should"
// is not a guarantee: misconfigured gateways and debug proxies echo request
// headers back, and once that text is inside an error it will be logged. The
// cost of scrubbing is one string scan on a path that is already failing.
//
// Both the bare key and its Bearer form are replaced. Short keys are ignored:
// below a few characters a "key" is far more likely to be a coincidental
// substring, and blanking those would corrupt legitimate error text.
func redactSecret(text, apiKey string) string {
	const minRedactable = 8
	if text == "" || len(apiKey) < minRedactable {
		return text
	}
	text = strings.ReplaceAll(text, "Bearer "+apiKey, "Bearer [REDACTED]")
	return strings.ReplaceAll(text, apiKey, "[REDACTED]")
}

// parseRetryAfter reads the Retry-After header, which providers send in two
// interchangeable forms: delay-seconds, or an HTTP date. Returns 0 when absent
// or unparseable — an unreadable hint is simply no hint, never an error.
//
// Absurd values are capped rather than trusted. A provider replying
// "Retry-After: 3600" during an incident would otherwise park the call for an
// hour inside what the caller believes is a bounded translate.
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return capRetryAfter(time.Duration(secs) * time.Second)
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d <= 0 {
			return 0
		}
		return capRetryAfter(d)
	}
	return 0
}

// maxRetryAfter bounds how long a provider may tell us to sleep. Past this the
// hint is worse than useless: the caller wanted an answer, and a fallback model
// or a clean error both beat a multi-minute stall.
const maxRetryAfter = 20 * time.Second

func capRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// asProviderError extracts a *ProviderError from an error chain, reporting
// whether one was found. Providers written by third parties return plain
// errors; those are treated as retryable transport failures by the retry loop,
// which is the safe default for an unknown cause.
func asProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// errModelUnreachable formats the terminal message for a call that never
// produced usable text, naming the knob that fixes it where one exists.
func errModelUnreachable(pe *ProviderError) error {
	switch pe.Kind {
	case failure.KindAuth:
		return fmt.Errorf("%w: check that the environment variable named by model.apiKeyEnv is exported "+
			"in THIS process and holds a key valid for this endpoint", pe)
	case failure.KindQuota:
		return fmt.Errorf("%w: this account is out of credit or past a billing cap; "+
			"waiting will not clear it — switch models or add a `models` fallback entry", pe)
	case failure.KindModelNotFound:
		return fmt.Errorf("%w: the endpoint does not serve this model id; check model.model for a typo "+
			"or a retired model", pe)
	default:
		return pe
	}
}
