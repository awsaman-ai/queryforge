package queryforge

import (
	"context"
	"math/rand"
	"time"
)

// Default retry/timeout settings. These are the values a model block gets when
// it says nothing, and they are chosen to be safe for the case QueryForge is
// most often pointed at: a free-tier key, where HTTP 429 is routine rather than
// exceptional.
const (
	// defaultTimeout bounds ONE round trip, matching http.Client.Timeout, which
	// is what this replaces. It is per-attempt rather than per-Complete because
	// the honest total bound is the caller's context: the SDKs already set one,
	// and a second total budget hidden in the model block would silently
	// contradict it.
	defaultTimeout = 30 * time.Second

	// defaultMaxRetries is the number of ADDITIONAL attempts after the first,
	// so the default is up to three round trips.
	//
	// Two, not zero: a rate-limited free tier recovers within a second or two,
	// and failing that call outright wastes a translation the user is waiting
	// on. Two, not more: every retry is spending someone's quota, and a chain
	// of `models` fallbacks multiplies this — three models at two retries is
	// already nine requests for one question.
	defaultMaxRetries = 2

	// defaultRetryBackoff is the first delay; each further retry doubles it.
	defaultRetryBackoff = 250 * time.Millisecond

	// maxRetryBackoff caps the computed (not provider-instructed) delay.
	maxRetryBackoff = 8 * time.Second
)

// retryPolicy decides whether and when a failed round trip is tried again.
//
// It is a value, not an interface, and it holds no state: the same policy is
// safe to share across goroutines, which matters because an Engine is.
type retryPolicy struct {
	MaxRetries  int           // additional attempts after the first; 0 disables retrying
	BaseBackoff time.Duration // first delay; doubles each retry

	// jitter randomises a computed delay. Injectable so tests can assert on
	// exact sleep arithmetic; nil means the real, randomised implementation.
	//
	// Jitter is not decoration. When a provider rate-limits, it usually
	// rate-limits every caller at once, and an un-jittered exponential curve
	// marches all of them back to the endpoint at the same instant — turning
	// one throttle into a self-inflicted thundering herd.
	jitter func(time.Duration) time.Duration
}

// retryPolicyFor builds the policy for a model block, applying defaults.
func retryPolicyFor(m ModelConfig) retryPolicy {
	return retryPolicy{
		MaxRetries:  m.EffectiveMaxRetries(),
		BaseBackoff: m.EffectiveRetryBackoff(),
	}
}

// equalJitter returns a delay in [d/2, d). Half the wait is guaranteed, so
// backoff still grows monotonically in expectation, and half is spread — which
// is what actually de-synchronises concurrent callers.
func equalJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int63n(int64(half)))
}

// delayFor computes how long to wait before retry number n (0-based).
//
// A provider's own Retry-After instruction wins over the computed curve, and is
// used unjittered: the provider stated a time, and second-guessing it either
// returns too early (another 429) or later than asked for no benefit.
func (rp retryPolicy) delayFor(n int, pe *ProviderError) time.Duration {
	if pe != nil && pe.RetryAfter > 0 {
		return pe.RetryAfter
	}

	base := rp.BaseBackoff
	if base <= 0 {
		base = defaultRetryBackoff
	}
	d := base
	for i := 0; i < n; i++ {
		d *= 2
		if d >= maxRetryBackoff {
			d = maxRetryBackoff
			break
		}
	}

	j := rp.jitter
	if j == nil {
		j = equalJitter
	}
	return j(d)
}

// shouldRetry reports whether a failed attempt gets another try, and how long to
// wait first.
//
// An error that is not a *ProviderError comes from a third-party ModelProvider
// implementation, whose failure modes this library cannot classify. Those are
// NOT retried: silently re-invoking someone else's provider could duplicate a
// side effect it performs, and a caller who wants retries around their own
// provider is better placed to write them than we are to guess.
func (rp retryPolicy) shouldRetry(attempt int, err error) (time.Duration, bool) {
	if attempt >= rp.MaxRetries {
		return 0, false
	}
	pe, ok := asProviderError(err)
	if !ok || !pe.Retryable() {
		return 0, false
	}
	return rp.delayFor(attempt, pe), true
}

// wait sleeps for d, returning false if the context ends first.
//
// It also declines to sleep at all when the context's deadline would expire
// during the wait. Sleeping through the remainder of the caller's budget
// converts an error they could have seen at once into a timeout that explains
// nothing — the caller loses both the answer and the diagnosis.
func wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(d).After(deadline) {
		return false
	}

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// do runs attempt until it succeeds, hits a non-retryable failure, or exhausts
// the retry budget. It returns the LAST error, which is the one describing the
// state the provider ended in.
//
// The round-trip index is passed to attempt so that each physical request can
// be reported separately to the Observer — the documented contract is one
// EventModelCall per round trip, and a retry is a round trip.
func (rp retryPolicy) do(ctx context.Context, attempt func(ctx context.Context, n int) (string, error)) (string, error) {
	var lastErr error
	for n := 0; ; n++ {
		// Check before spending a request, so a context that ended during the
		// previous backoff does not produce one more doomed call.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return "", lastErr // the provider failure is the useful diagnosis
			}
			return "", err
		}

		out, err := attempt(ctx, n)
		if err == nil {
			return out, nil
		}
		lastErr = err

		delay, ok := rp.shouldRetry(n, err)
		if !ok {
			return "", err
		}
		if !wait(ctx, delay) {
			return "", err
		}
	}
}
