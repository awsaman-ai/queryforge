package queryforge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ProviderErrorKind classifies why a model call failed, at a granularity the
// retry loop can act on.
//
// The library already classifies failures for the CALLER via FailureCode
// (MODEL_TRANSPORT vs MODEL_OUTPUT vs …), and that taxonomy is a wire contract
// shared with the Java and Python SDKs — adding members to it would break older
// SDKs that map codes to exception classes. This kind is deliberately a
// SEPARATE, Go-only axis: it answers "should we try again?", not "what should
// the caller be told?". A ProviderError still reaches the planner as an ordinary
// error and is still wrapped into ErrModelTransport, so FailureCode is unchanged.
type ProviderErrorKind string

const (
	// KindAuth means the endpoint rejected the credential: missing, malformed,
	// revoked, or not entitled to this model. Retrying sends the same bad key.
	KindAuth ProviderErrorKind = "AUTH"

	// KindQuota means the account is out of credit or past a hard billing cap.
	// Distinct from KindRateLimit because waiting does not help — only a
	// different provider (or a different wallet) does. With a fallback chain
	// configured this should hand over immediately rather than sleep first.
	KindQuota ProviderErrorKind = "QUOTA"

	// KindRateLimit means too many requests in the window. This is the one
	// failure where backing off and trying again genuinely fixes the problem,
	// and it is the failure free-tier keys hit most.
	KindRateLimit ProviderErrorKind = "RATE_LIMIT"

	// KindModelNotFound means the endpoint does not serve the configured model
	// id — a typo, or a model that has been retired. Retrying cannot conjure it.
	KindModelNotFound ProviderErrorKind = "MODEL_NOT_FOUND"

	// KindInvalidRequest means the endpoint rejected the request body itself.
	// The body is deterministic for a given config, so a retry reproduces it
	// byte for byte.
	KindInvalidRequest ProviderErrorKind = "INVALID_REQUEST"

	// KindUnavailable means the provider failed on its own side (5xx) or is
	// overloaded. Transient by definition, so worth another attempt.
	KindUnavailable ProviderErrorKind = "UNAVAILABLE"

	// KindTimeout means the request exceeded its deadline. Retryable only when
	// the budget that expired was the PER-ATTEMPT one; see Retryable.
	KindTimeout ProviderErrorKind = "TIMEOUT"

	// KindTransport means the request never completed at the network level —
	// DNS failure, refused connection, TLS error, dropped socket.
	KindTransport ProviderErrorKind = "TRANSPORT"

	// KindBadResponse means the endpoint answered 2xx but the payload was not
	// usable: undecodable JSON, no choices, or a reply truncated by the token
	// budget. Not retried HERE — a truncated reply truncates again, and genuine
	// model-output problems belong to the engine's repair loop, which can
	// change the prompt. Retrying at this layer would silently multiply the
	// repair budget.
	KindBadResponse ProviderErrorKind = "BAD_RESPONSE"
)

// Retryable reports whether another identical attempt could plausibly succeed.
//
// The rule is deliberately narrow: retry only failures whose cause is TIME
// (the provider was busy, throttling, or briefly broken). Everything caused by
// the request itself — a bad key, an unknown model, a malformed body, an empty
// wallet — reproduces exactly on the next attempt, so retrying it only delays
// the error the caller needs to see, and on a rate-limited key it spends
// quota to learn nothing.
func (k ProviderErrorKind) Retryable() bool {
	switch k {
	case KindRateLimit, KindUnavailable, KindTimeout, KindTransport:
		return true
	default:
		return false
	}
}

// ProviderError is a model-call failure with its cause classified. It is what
// every built-in provider returns, so the retry loop can decide without parsing
// error strings.
//
// It carries no credential. See newProviderError: the response snippet is
// redacted before it is ever stored.
type ProviderError struct {
	Kind     ProviderErrorKind // why it failed, and whether time might fix it
	Status   int               // HTTP status, or 0 when the call never got one
	Provider string            // provider label, for the message
	Model    string            // model id, for the message
	Detail   string            // redacted, bounded description
	Err      error             // wrapped cause, if any

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
func classifyStatus(status int, body string) ProviderErrorKind {
	switch status {
	case http.StatusUnauthorized: // 401
		return KindAuth

	case http.StatusForbidden: // 403
		// Ambiguous in practice: some providers use 403 for a revoked key and
		// others for an exhausted free tier. A quota hint decides it, because
		// the two want opposite handling — fail over vs. fail fast to the user.
		if looksLikeQuota(body) {
			return KindQuota
		}
		return KindAuth

	case http.StatusPaymentRequired: // 402
		return KindQuota

	case http.StatusNotFound: // 404
		return KindModelNotFound

	case http.StatusRequestTimeout: // 408
		return KindTimeout

	case http.StatusTooManyRequests: // 429
		// A 429 usually means "slow down" (retry) but several providers reuse
		// it for "you are out of credit" (do not retry — no amount of waiting
		// adds funds). Only a quota hint distinguishes them.
		if looksLikeQuota(body) {
			return KindQuota
		}
		return KindRateLimit
	}

	switch {
	case status >= 500:
		return KindUnavailable
	case status >= 400:
		// 400, 413, 422 and friends: the endpoint understood us and said no.
		return KindInvalidRequest
	default:
		return KindBadResponse
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
func classifyTransport(ctx context.Context, err error) ProviderErrorKind {
	// The parent context expiring is not the provider's fault and is not
	// retryable: the caller's whole budget is gone, so a further attempt has
	// nowhere to run. A per-attempt deadline expiring IS retryable, and that
	// case is distinguished by the caller's context still being live.
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return KindTransport
		}
		return KindTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	if errors.Is(err, context.Canceled) {
		return KindTransport
	}
	return KindTransport
}

// newProviderError builds a classified error, redacting the credential from the
// detail text before it is stored.
//
// Redaction happens HERE, at construction, rather than at logging time. An error
// travels further than any logger: into a caller's own logs, an exception
// message, a bug report, a crash dump. Scrubbing at the single point where the
// text is created means there is no path by which the raw key can escape,
// including paths this library does not control.
func newProviderError(kind ProviderErrorKind, status int, providerID, model, detail, apiKey string, err error) *ProviderError {
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
	case KindAuth:
		return fmt.Errorf("%w: check that the environment variable named by model.apiKeyEnv is exported "+
			"in THIS process and holds a key valid for this endpoint", pe)
	case KindQuota:
		return fmt.Errorf("%w: this account is out of credit or past a billing cap; "+
			"waiting will not clear it — switch models or add a `models` fallback entry", pe)
	case KindModelNotFound:
		return fmt.Errorf("%w: the endpoint does not serve this model id; check model.model for a typo "+
			"or a retired model", pe)
	default:
		return pe
	}
}
