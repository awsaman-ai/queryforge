package queryforge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noJitter makes backoff arithmetic assertable. Real jitter is exercised
// separately in TestJitterStaysWithinItsBand.
func noJitter(d time.Duration) time.Duration { return d }

// fastRetry is a policy with no real sleeping, for tests about COUNTS rather
// than timing.
func fastRetry(max int) retryPolicy {
	return retryPolicy{MaxRetries: max, BaseBackoff: time.Microsecond, jitter: noJitter}
}

// ---------------------------------------------------------------- decisions

// TestRetryHappensOnlyForFailuresTimeCanFix is the central rule. Each case
// states the number of round trips the caller should pay for.
func TestRetryHappensOnlyForFailuresTimeCanFix(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCalls  int
		wantReason string
	}{
		{
			name: "rate limit is retried", err: &ProviderError{Kind: KindRateLimit},
			wantCalls: 3, wantReason: "waiting out a throttle is the case retrying exists for",
		},
		{
			name: "provider outage is retried", err: &ProviderError{Kind: KindUnavailable},
			wantCalls: 3, wantReason: "a 5xx is transient by definition",
		},
		{
			name: "dropped connection is retried", err: &ProviderError{Kind: KindTransport},
			wantCalls: 3,
		},
		{
			name: "per-attempt timeout is retried", err: &ProviderError{Kind: KindTimeout},
			wantCalls: 3,
		},
		{
			name: "bad key fails immediately", err: &ProviderError{Kind: KindAuth},
			wantCalls: 1, wantReason: "the same key would be resent; retrying spends rate limit to learn nothing",
		},
		{
			name: "empty wallet fails immediately", err: &ProviderError{Kind: KindQuota},
			wantCalls: 1, wantReason: "no amount of waiting adds credit",
		},
		{
			name: "unknown model fails immediately", err: &ProviderError{Kind: KindModelNotFound},
			wantCalls: 1,
		},
		{
			name: "malformed request fails immediately", err: &ProviderError{Kind: KindInvalidRequest},
			wantCalls: 1, wantReason: "the request body is deterministic",
		},
		{
			name: "truncated reply fails immediately", err: &ProviderError{Kind: KindBadResponse},
			wantCalls: 1, wantReason: "an identical request truncates identically",
		},
		{
			name:       "an unclassifiable third-party error is not retried",
			err:        errors.New("something went wrong in a custom provider"),
			wantCalls:  1,
			wantReason: "a third-party ModelProvider may have side effects we must not repeat blindly",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := fastRetry(2).do(context.Background(), func(context.Context, int) (string, error) {
				calls++
				return "", tc.err
			})
			if err == nil {
				t.Fatal("expected the failure to be reported")
			}
			if calls != tc.wantCalls {
				t.Errorf("made %d round trips, want %d — %s", calls, tc.wantCalls, tc.wantReason)
			}
		})
	}
}

// TestRetryStopsAtTheFirstSuccess: the budget is a ceiling, not a quota to
// spend. A call that succeeds on the second attempt must make exactly two.
func TestRetryStopsAtTheFirstSuccess(t *testing.T) {
	calls := 0
	out, err := fastRetry(5).do(context.Background(), func(context.Context, int) (string, error) {
		calls++
		if calls < 2 {
			return "", &ProviderError{Kind: KindRateLimit}
		}
		return "the answer", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "the answer" {
		t.Errorf("got %q, want the successful reply", out)
	}
	if calls != 2 {
		t.Errorf("made %d round trips, want exactly 2", calls)
	}
}

// TestRetriesAreBounded: the loop must terminate on a permanently failing
// endpoint. An unbounded retry turns one bad provider into a hung caller.
func TestRetriesAreBounded(t *testing.T) {
	for _, max := range []int{0, 1, 2, 5} {
		calls := 0
		_, err := fastRetry(max).do(context.Background(), func(context.Context, int) (string, error) {
			calls++
			return "", &ProviderError{Kind: KindUnavailable}
		})
		if err == nil {
			t.Fatal("expected an error")
		}
		if want := max + 1; calls != want {
			t.Errorf("MaxRetries=%d made %d round trips, want %d", max, calls, want)
		}
	}
}

// TestZeroRetriesMeansExactlyOneAttempt: an explicit 0 is a real setting — some
// callers want to fail over to the next model instantly rather than wait.
func TestZeroRetriesMeansExactlyOneAttempt(t *testing.T) {
	calls := 0
	_, _ = retryPolicy{MaxRetries: 0}.do(context.Background(), func(context.Context, int) (string, error) {
		calls++
		return "", &ProviderError{Kind: KindRateLimit}
	})
	if calls != 1 {
		t.Errorf("made %d round trips, want 1", calls)
	}
}

// TestTheLastErrorIsWhatTheCallerSees: the final state of the provider is the
// useful diagnosis, not the first blip on the way there.
func TestTheLastErrorIsWhatTheCallerSees(t *testing.T) {
	final := &ProviderError{Kind: KindAuth, Detail: "key revoked mid-flight"}
	calls := 0
	_, err := fastRetry(3).do(context.Background(), func(context.Context, int) (string, error) {
		calls++
		if calls < 2 {
			return "", &ProviderError{Kind: KindUnavailable, Detail: "first blip"}
		}
		return "", final
	})
	if !strings.Contains(err.Error(), "key revoked mid-flight") {
		t.Errorf("got %v, want the final error", err)
	}
}

// TestTheRoundTripIndexIsPassedThrough: each physical request is reported to the
// Observer separately, which needs its own index.
func TestTheRoundTripIndexIsPassedThrough(t *testing.T) {
	var seen []int
	_, _ = fastRetry(2).do(context.Background(), func(_ context.Context, n int) (string, error) {
		seen = append(seen, n)
		return "", &ProviderError{Kind: KindUnavailable}
	})
	for i, n := range seen {
		if n != i {
			t.Errorf("round trip %d was told it was attempt %d", i, n)
		}
	}
}

// ------------------------------------------------------------------ backoff

// TestBackoffGrowsAndIsCapped: the delay must rise so a struggling provider gets
// progressively more room, and must stop rising so a caller is never parked.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	rp := retryPolicy{MaxRetries: 10, BaseBackoff: 100 * time.Millisecond, jitter: noJitter}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		if got := rp.delayFor(i, nil); got != w {
			t.Errorf("delayFor(%d) = %v, want %v", i, got, w)
		}
	}

	if got := rp.delayFor(20, nil); got != maxRetryBackoff {
		t.Errorf("delayFor(20) = %v, want it capped at %v", got, maxRetryBackoff)
	}
}

// TestProviderRetryAfterBeatsComputedBackoff: a provider that states when to
// come back knows better than an exponential curve. Returning earlier just
// earns another 429; returning later wastes the caller's time.
func TestProviderRetryAfterBeatsComputedBackoff(t *testing.T) {
	rp := retryPolicy{MaxRetries: 3, BaseBackoff: time.Second, jitter: noJitter}
	pe := &ProviderError{Kind: KindRateLimit, RetryAfter: 2 * time.Second}

	if got := rp.delayFor(0, pe); got != 2*time.Second {
		t.Errorf("delayFor with Retry-After = %v, want the provider's 2s", got)
	}
	// And it must not be jittered away from what the provider asked for.
	if got := (retryPolicy{BaseBackoff: time.Second}).delayFor(0, pe); got != 2*time.Second {
		t.Errorf("Retry-After was jittered to %v; it must be used verbatim", got)
	}
}

// TestJitterStaysWithinItsBand: jitter exists so that many callers throttled at
// the same instant do not all return at the same instant. It must therefore
// actually vary, while still guaranteeing at least half the intended wait.
func TestJitterStaysWithinItsBand(t *testing.T) {
	const d = 800 * time.Millisecond
	seen := map[time.Duration]bool{}

	for i := 0; i < 200; i++ {
		got := equalJitter(d)
		if got < d/2 || got >= d {
			t.Fatalf("jittered delay %v outside [%v, %v)", got, d/2, d)
		}
		seen[got] = true
	}
	if len(seen) < 10 {
		t.Errorf("jitter produced only %d distinct delays over 200 draws — "+
			"too clustered to de-synchronise concurrent callers", len(seen))
	}
}

// -------------------------------------------------------- context behaviour

// TestBackoffDoesNotOutlastTheCallersDeadline: sleeping through the remainder
// of the caller's budget converts an error they could have seen at once into a
// timeout that explains nothing.
func TestBackoffDoesNotOutlastTheCallersDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	calls := 0
	started := time.Now()
	rp := retryPolicy{MaxRetries: 5, BaseBackoff: 10 * time.Second, jitter: noJitter}
	_, err := rp.do(ctx, func(context.Context, int) (string, error) {
		calls++
		return "", &ProviderError{Kind: KindRateLimit, Detail: "throttled"}
	})

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("waited %v; must not sleep past the caller's deadline", elapsed)
	}
	if calls != 1 {
		t.Errorf("made %d round trips, want 1 — there was no budget for a second", calls)
	}
	if !strings.Contains(err.Error(), "throttled") {
		t.Errorf("got %v, want the provider's own failure rather than a bare deadline error", err)
	}
}

// TestCancellationStopsTheLoopPromptly: a cancelled caller must not be kept
// waiting through a backoff, and must not be charged for another request.
func TestCancellationStopsTheLoopPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := int32(0)
	rp := retryPolicy{MaxRetries: 5, BaseBackoff: 2 * time.Second, jitter: noJitter}

	started := time.Now()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := rp.do(ctx, func(context.Context, int) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", &ProviderError{Kind: KindUnavailable}
	})

	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("took %v to notice cancellation", elapsed)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("made %d round trips after cancellation, want 1", n)
	}
}

// TestAnAlreadyDeadContextMakesNoRequest: spending a request that cannot
// possibly return in time is pure waste, and on a metered API it is billable
// waste.
func TestAnAlreadyDeadContextMakesNoRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	_, err := fastRetry(2).do(ctx, func(context.Context, int) (string, error) {
		calls++
		return "", nil
	})
	if calls != 0 {
		t.Errorf("made %d round trips on a dead context, want 0", calls)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

// ------------------------------------------------------ end-to-end over HTTP

// TestProviderRecoversFromAThrottleEndToEnd is the whole point, exercised
// through the real HTTP path: a provider that 429s once and then answers must
// produce an answer, not an error.
func TestProviderRecoversFromAThrottleEndToEnd(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "rate limited")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m", RetryBackoffMs: 1})
	out, err := p.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("a single throttle should have been ridden out, got: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("got %q, want the eventual successful reply", out)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("endpoint saw %d requests, want 2", n)
	}
}

// TestProviderDoesNotRetryABadKeyEndToEnd: the counterpart. A 401 must cost
// exactly one request no matter what the retry budget says.
func TestProviderDoesNotRetryABadKeyEndToEnd(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m", RetryBackoffMs: 1})
	_, err := p.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("endpoint saw %d requests, want exactly 1", n)
	}

	pe, ok := asProviderError(err)
	if !ok || pe.Kind != KindAuth {
		t.Errorf("error was not classified as AUTH: %v", err)
	}
}

// TestRetryIsSeparateFromTheRepairBudget: transport retries must not consume
// the engine's validation-repair attempts, and vice versa. Conflating them
// would let a throttled endpoint silently eat the repair budget.
func TestRetryIsSeparateFromTheRepairBudget(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always throttle: the transport layer should exhaust its own budget
		// and give up without the engine ever seeing model output to repair.
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "overloaded")
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m", RetryBackoffMs: 1})
	if _, err := p.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected an error")
	}

	// One Complete = at most 1 + MaxRetries round trips. Anything more means a
	// second loop is multiplying the first.
	if n := atomic.LoadInt32(&hits); n != int32(defaultMaxRetries+1) {
		t.Errorf("one Complete produced %d requests, want %d", n, defaultMaxRetries+1)
	}
}

// TestHandConstructedProviderDoesNotRetry pins backward compatibility: before
// these fields existed, a provider built by struct literal made exactly one
// request. Zero-valued MaxRetries must preserve that exactly.
func TestHandConstructedProviderDoesNotRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := &OpenAIProvider{BaseURL: srv.URL, Model: "m"} // no MaxRetries set
	if _, err := p.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected an error")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("a hand-built provider made %d requests, want 1 (unchanged behaviour)", n)
	}
}

// TestFallbackChainMovesOnWithoutRetryingWhenItCannotHelp: the two resilience
// mechanisms have to compose sensibly. A bad key on the primary should reach
// the secondary immediately rather than after three throttle-style waits.
func TestFallbackChainMovesOnWithoutRetryingWhenItCannotHelp(t *testing.T) {
	var primaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"answered"},"finish_reason":"stop"}]}`)
	}))
	defer secondary.Close()

	cfg := &Config{
		Model:  ModelConfig{Provider: "primary", BaseURL: primary.URL, Model: "a", RetryBackoffMs: 1},
		Models: []ModelConfig{{Provider: "secondary", BaseURL: secondary.URL, Model: "b", RetryBackoffMs: 1}},
	}
	out, err := ProvidersFrom(cfg).Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("the chain should have fallen through to the healthy model: %v", err)
	}
	if out != "answered" {
		t.Errorf("got %q, want the secondary's reply", out)
	}
	if n := atomic.LoadInt32(&primaryHits); n != 1 {
		t.Errorf("primary was called %d times; an auth failure must hand over at once", n)
	}
}
