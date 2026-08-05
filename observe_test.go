package queryforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// recorder is a thread-safe Observer that keeps every event it is handed. It is
// locked because several tests below run translations concurrently, and an
// Observer that is not safe for concurrent use would turn a race in the library
// into a race report about the test.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) observe() Observer {
	return func(_ context.Context, e Event) {
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
	}
}

// all returns a copy of the recorded events.
func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// kind returns only the events of one kind, in order.
func (r *recorder) kind(k EventKind) []Event {
	var out []Event
	for _, e := range r.all() {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// kinds renders the observed sequence, for readable failure messages.
func (r *recorder) kinds() []EventKind {
	var out []EventKind
	for _, e := range r.all() {
		out = append(out, e.Kind)
	}
	return out
}

// erroringProvider always fails, standing in for an unreachable endpoint.
type erroringProvider struct{ err error }

func (p *erroringProvider) Complete(_ context.Context, _, _ string) (string, error) {
	return "", p.err
}

// Canned model replies used across the tests below.
const (
	// validAST parses and satisfies every rule in genConfigJSON.
	validAST = `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	// invalidAST parses but names a field the config does not declare.
	invalidAST = `{"entity":"Order","filter":{"type":"comparison","field":"stat","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	// unparseableReply contains no JSON object at all.
	unparseableReply = `I'm sorry, I cannot help with that.`
	// refusalReply is the model's deliberate "not in this vocabulary" answer.
	refusalReply = `{"unsupported":"no field for shipping warehouse"}`
)

// ─────────────────────────────────────────────────────────────────────────────
// Happy path
// ─────────────────────────────────────────────────────────────────────────────

// TestObserverHappyPathSequence: a clean translation emits one attempt event
// and one translate event, both OK, in that order.
func TestObserverHappyPathSequence(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(rec.observe())

	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(got), rec.kinds())
	}
	if got[0].Kind != EventAttempt || got[0].Outcome != OutcomeOK {
		t.Errorf("first event = %s/%s, want attempt/ok", got[0].Kind, got[0].Outcome)
	}
	if got[1].Kind != EventTranslate || got[1].Outcome != OutcomeOK {
		t.Errorf("second event = %s/%s, want translate/ok", got[1].Kind, got[1].Outcome)
	}
	// Fields every kind must carry.
	for _, ev := range got {
		if ev.Entity != "Order" || ev.Backend != "sql" {
			t.Errorf("%s missing entity/backend: %q/%q", ev.Kind, ev.Entity, ev.Backend)
		}
	}
	if got[1].Duration <= 0 {
		t.Error("translate event should carry a positive duration")
	}
}

// TestObserverAttemptNumberingMatchesResult: the attempt index on the events and
// TranslateResult.RepairAttempts must agree, or a log cannot be reconciled with
// what the caller was told.
func TestObserverAttemptNumberingMatchesResult(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &scriptedProvider{responses: []string{invalidAST, invalidAST, validAST}})
	e.SetObserver(rec.observe())

	res, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
	if err != nil {
		t.Fatalf("Translate should have recovered: %v", err)
	}

	attempts := rec.kind(EventAttempt)
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempt events, got %d", len(attempts))
	}
	for i, ev := range attempts {
		if ev.Attempt != i {
			t.Errorf("attempt event %d reports Attempt=%d", i, ev.Attempt)
		}
	}
	// The two failures, then the success.
	if attempts[0].Outcome != OutcomeValidation || attempts[1].Outcome != OutcomeValidation {
		t.Errorf("first two attempts = %s/%s, want validation_error twice", attempts[0].Outcome, attempts[1].Outcome)
	}
	if attempts[2].Outcome != OutcomeOK {
		t.Errorf("final attempt = %s, want ok", attempts[2].Outcome)
	}

	final := rec.kind(EventTranslate)
	if len(final) != 1 {
		t.Fatalf("expected 1 translate event, got %d", len(final))
	}
	if final[0].RepairAttempts != res.RepairAttempts {
		t.Errorf("event RepairAttempts=%d, result RepairAttempts=%d — must agree",
			final[0].RepairAttempts, res.RepairAttempts)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Outcome classification — one case per constant the engine can produce
// ─────────────────────────────────────────────────────────────────────────────

func TestObserverOutcomeClassification(t *testing.T) {
	cases := []struct {
		name        string
		provider    ModelProvider
		backend     string
		scope       Scope
		wantAttempt Outcome // zero value "" means: expect no attempt event
		wantFinal   Outcome
	}{
		{
			name:      "unknown backend is a caller error, decided before any model call",
			provider:  &StubProvider{Response: canonicalAST},
			backend:   "graphql",
			wantFinal: OutcomeCallerError,
		},
		{
			name:      "bad scope is a caller error",
			provider:  &StubProvider{Response: canonicalAST},
			backend:   "sql",
			scope:     Scope{"tenantId": nil}, // nil value is never a valid filter
			wantFinal: OutcomeCallerError,
		},
		{
			name:        "transport failure is final, not repairable",
			provider:    &erroringProvider{err: errors.New("connection refused")},
			backend:     "sql",
			wantAttempt: OutcomeTransport,
			wantFinal:   OutcomeTransport,
		},
		{
			name:        "a deliberate refusal is a real answer, not a fault",
			provider:    &StubProvider{Response: refusalReply},
			backend:     "sql",
			wantAttempt: OutcomeRefusal,
			wantFinal:   OutcomeRefusal,
		},
		{
			name:        "unparseable output is repairable, then exhausts the budget",
			provider:    &StubProvider{Response: unparseableReply},
			backend:     "sql",
			wantAttempt: OutcomeParseError,
			wantFinal:   OutcomeBudgetSpent,
		},
		{
			name:        "an AST that breaks a config rule exhausts the budget",
			provider:    &StubProvider{Response: invalidAST},
			backend:     "sql",
			wantAttempt: OutcomeValidation,
			wantFinal:   OutcomeBudgetSpent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			e := newTestEngine(t, tc.provider)
			e.SetObserver(rec.observe())

			if _, err := e.Translate(context.Background(), "some question", tc.backend, tc.scope); err == nil {
				t.Fatal("expected an error from this case")
			}

			attempts := rec.kind(EventAttempt)
			if tc.wantAttempt == "" {
				if len(attempts) != 0 {
					t.Errorf("expected no attempt events (failure precedes the model call), got %d", len(attempts))
				}
			} else {
				if len(attempts) == 0 {
					t.Fatal("expected at least one attempt event")
				}
				if attempts[0].Outcome != tc.wantAttempt {
					t.Errorf("first attempt outcome = %s, want %s", attempts[0].Outcome, tc.wantAttempt)
				}
			}

			final := rec.kind(EventTranslate)
			if len(final) != 1 {
				t.Fatalf("expected exactly 1 translate event, got %d", len(final))
			}
			if final[0].Outcome != tc.wantFinal {
				t.Errorf("translate outcome = %s, want %s", final[0].Outcome, tc.wantFinal)
			}
			if final[0].Err == nil {
				t.Error("a failed translate event must carry the error")
			}
		})
	}
}

// TestObserverExactlyOneTranslateEventPerCall walks every exit path and asserts
// the invariant the design promises: a caller counting events never sees a
// translation that started and never ended.
func TestObserverExactlyOneTranslateEventPerCall(t *testing.T) {
	paths := []struct {
		name     string
		provider ModelProvider
		backend  string
		scope    Scope
	}{
		{"success", &StubProvider{Response: canonicalAST}, "sql", nil},
		{"success with scope", &StubProvider{Response: canonicalAST}, "sql", Scope{"tenantId": "T-1"}},
		{"unknown backend", &StubProvider{Response: canonicalAST}, "graphql", nil},
		{"bad scope", &StubProvider{Response: canonicalAST}, "sql", Scope{"tenantId": nil}},
		{"transport failure", &erroringProvider{err: errors.New("boom")}, "sql", nil},
		{"refusal", &StubProvider{Response: refusalReply}, "sql", nil},
		{"budget exhausted on parse", &StubProvider{Response: unparseableReply}, "sql", nil},
		{"budget exhausted on validation", &StubProvider{Response: invalidAST}, "sql", nil},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			var rec recorder
			e := newTestEngine(t, p.provider)
			e.SetObserver(rec.observe())

			_, _ = e.Translate(context.Background(), "question", p.backend, p.scope)

			if n := len(rec.kind(EventTranslate)); n != 1 {
				t.Errorf("got %d translate events, want exactly 1 (sequence: %v)", n, rec.kinds())
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Privacy — §5 of observability_plan.md, enforced rather than trusted
// ─────────────────────────────────────────────────────────────────────────────

// TestObserverNeverLeaksQuestionOrScopeValues puts a canary in both the question
// and the scope value, then asserts it appears in no field of any event — with
// the single documented exception of Raw on a failed attempt, which can echo the
// question and is why the service logs it at debug only.
func TestObserverNeverLeaksQuestionOrScopeValues(t *testing.T) {
	const questionCanary = "CANARY-QUESTION-8471"
	const scopeCanary = "CANARY-SCOPE-3390"

	// Run both a success and a failure, since they populate different fields.
	for _, tc := range []struct {
		name     string
		provider ModelProvider
	}{
		{"success", &StubProvider{Response: canonicalAST}},
		{"validation failure", &StubProvider{Response: invalidAST}},
		{"transport failure", &erroringProvider{err: errors.New("boom")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			e := newTestEngine(t, tc.provider)
			e.SetObserver(rec.observe())

			_, _ = e.Translate(context.Background(),
				"orders about "+questionCanary, "sql", Scope{"tenantId": scopeCanary})

			for i, ev := range rec.all() {
				// Raw is the one field allowed to contain caller data, and only
				// on a failed attempt. Everything else is checked.
				scrubbed := ev
				scrubbed.Raw = ""
				dump := fmt.Sprintf("%+v", scrubbed)
				if strings.Contains(dump, questionCanary) {
					t.Errorf("event %d (%s) leaked the question text: %s", i, ev.Kind, dump)
				}
				if strings.Contains(dump, scopeCanary) {
					t.Errorf("event %d (%s) leaked a scope VALUE: %s", i, ev.Kind, dump)
				}
				// Raw must never appear on a successful attempt or on the final event.
				if ev.Raw != "" && (ev.Kind != EventAttempt || ev.Outcome == OutcomeOK) {
					t.Errorf("event %d (%s/%s) carries Raw, which is only allowed on a failed attempt",
						i, ev.Kind, ev.Outcome)
				}
			}
		})
	}
}

// TestObserverReportsScopeKeysWithoutValues: the field NAMES are useful in an
// audit trail and are emitted; the values are not.
func TestObserverReportsScopeKeysWithoutValues(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(rec.observe())

	if _, err := e.Translate(context.Background(), "delivered orders", "sql",
		Scope{"tenantId": "T-1", "userId": "U-9"}); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	final := rec.kind(EventTranslate)[0]
	if len(final.ScopeKeys) != 2 {
		t.Fatalf("ScopeKeys = %v, want 2 entries", final.ScopeKeys)
	}
	joined := strings.Join(final.ScopeKeys, ",")
	if !strings.Contains(joined, "tenantId") || !strings.Contains(joined, "userId") {
		t.Errorf("ScopeKeys = %v, want the two field names", final.ScopeKeys)
	}
}

// TestObserverRawCarriesTheModelReplyOnFailure: the whole point of Raw. Without
// it a parse failure says only THAT the reply was unusable, never what it said.
func TestObserverRawCarriesTheModelReplyOnFailure(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &StubProvider{Response: unparseableReply})
	e.SetObserver(rec.observe())

	_, _ = e.Translate(context.Background(), "delivered orders", "sql", nil)

	attempts := rec.kind(EventAttempt)
	if len(attempts) == 0 {
		t.Fatal("expected attempt events")
	}
	if attempts[0].Raw != unparseableReply {
		t.Errorf("Raw = %q, want the model's verbatim reply", attempts[0].Raw)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Adversarial
// ─────────────────────────────────────────────────────────────────────────────

// TestNilObserverIsTheDefaultAndIsSafe: no Observer is the configuration that
// runs everywhere, so it must work on every path without a nil dereference.
func TestNilObserverIsTheDefaultAndIsSafe(t *testing.T) {
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	if e.Observe != nil {
		t.Fatal("Observe must default to nil")
	}
	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
		t.Fatalf("Translate with nil observer: %v", err)
	}

	// And on the failure paths, which emit from different call sites.
	bad := newTestEngine(t, &StubProvider{Response: invalidAST})
	if _, err := bad.Translate(context.Background(), "delivered orders", "sql", nil); err == nil {
		t.Fatal("expected failure")
	}

	// Calling the zero Observer directly must also be safe.
	var o Observer
	o.emit(context.Background(), Event{Kind: EventAttempt})
}

// TestObserverPanicPropagates pins the documented contract: a panicking Observer
// is a bug in the CALLER, and recovering it would produce a system that silently
// stops reporting — strictly worse than a loud failure.
func TestObserverPanicPropagates(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("a panicking Observer must not be swallowed by the library")
		}
	}()

	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(func(context.Context, Event) { panic("observer blew up") })

	_, _ = e.Translate(context.Background(), "delivered orders", "sql", nil)
	t.Error("unreachable: the panic should have propagated")
}

// TestObserverIsCalledSynchronously: the doc comment promises the call happens
// on the hot path, which is what lets a caller log Raw next to its request. If
// delivery ever became async this test would see an empty recorder.
func TestObserverIsCalledSynchronously(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(rec.observe())

	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(rec.all()) == 0 {
		t.Error("events must have been delivered by the time Translate returns")
	}
}

// TestObserverReceivesTheCallerContext: the reason Observer takes a context at
// all. A caller stamps a request id on the context it passes to Translate and
// must be able to read it back on every event — without that, concurrent
// requests produce interleaved log lines that cannot be grouped.
func TestObserverReceivesTheCallerContext(t *testing.T) {
	type ridKey struct{}

	var mu sync.Mutex
	seen := map[string]int{} // request id -> events observed under it

	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(func(ctx context.Context, _ Event) {
		rid, _ := ctx.Value(ridKey{}).(string)
		mu.Lock()
		seen[rid]++
		mu.Unlock()
	})

	// Two concurrent translations under different ids: every event must land
	// under the id of the call that produced it, never the other one.
	var wg sync.WaitGroup
	for _, rid := range []string{"req-A", "req-B"} {
		wg.Add(1)
		go func(rid string) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), ridKey{}, rid)
			if _, err := e.Translate(ctx, "delivered orders", "sql", nil); err != nil {
				t.Errorf("Translate: %v", err)
			}
		}(rid)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("events landed under %d ids, want 2: %v", len(seen), seen)
	}
	for _, rid := range []string{"req-A", "req-B"} {
		if seen[rid] != 2 { // one attempt + one translate
			t.Errorf("id %q saw %d events, want 2", rid, seen[rid])
		}
	}
}

// TestObserverUnderConcurrentTranslate: one Engine serves many goroutines, so
// events from different translations interleave. Run under -race (the suite
// already does) this is the guard against the seam introducing shared state.
func TestObserverUnderConcurrentTranslate(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(rec.observe())

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
				t.Errorf("Translate: %v", err)
			}
		}()
	}
	wg.Wait()

	// Two events per translation, and no more — a duplicated or dropped emission
	// would show up here as a count mismatch.
	if n := len(rec.kind(EventTranslate)); n != goroutines {
		t.Errorf("got %d translate events for %d translations", n, goroutines)
	}
	if n := len(rec.kind(EventAttempt)); n != goroutines {
		t.Errorf("got %d attempt events for %d translations", n, goroutines)
	}
}

// TestHiddenTokensClampsAtZero: a provider that omits usage, or reports a total
// smaller than its parts, must never produce a negative count in a log.
func TestHiddenTokensClampsAtZero(t *testing.T) {
	cases := []struct {
		total, prompt, completion, want int
	}{
		{704, 590, 114, 0},    // no reasoning tokens (flash-lite)
		{1280, 590, 114, 576}, // hidden reasoning dominates the visible answer
		{0, 0, 0, 0},          // provider omitted the usage block entirely
		{10, 50, 50, 0},       // nonsense from the provider: clamp, never negative
	}
	for _, c := range cases {
		if got := hiddenTokens(c.total, c.prompt, c.completion); got != c.want {
			t.Errorf("hiddenTokens(%d,%d,%d) = %d, want %d",
				c.total, c.prompt, c.completion, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Model-call events — the facts only the provider can see
// ─────────────────────────────────────────────────────────────────────────────

// TestModelCallEventCarriesUsageAndLatency: tokens, finish reason, model id and
// latency are the reason this seam exists at all.
func TestModelCallEventCarriesUsageAndLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":590,"completion_tokens":114,"total_tokens":1280}}`)
	}))
	defer srv.Close()

	var rec recorder
	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "gemini-3.5-flash", Provider: "gemini"})
	p.SetObserver(rec.observe())

	if _, err := p.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	calls := rec.kind(EventModelCall)
	if len(calls) != 1 {
		t.Fatalf("expected 1 model_call event, got %d", len(calls))
	}
	ev := calls[0]
	if ev.Outcome != OutcomeOK {
		t.Errorf("outcome = %s, want ok", ev.Outcome)
	}
	if ev.Provider != "gemini" || ev.Model != "gemini-3.5-flash" {
		t.Errorf("provider/model = %q/%q", ev.Provider, ev.Model)
	}
	if ev.PromptTokens != 590 || ev.CompletionTokens != 114 || ev.TotalTokens != 1280 {
		t.Errorf("token counts wrong: %+v", ev)
	}
	// 1280 - 590 - 114: the hidden reasoning bill, invisible in the reply.
	if ev.HiddenTokens != 576 {
		t.Errorf("HiddenTokens = %d, want 576", ev.HiddenTokens)
	}
	if ev.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", ev.FinishReason)
	}
	if ev.Latency <= 0 {
		t.Error("latency must be positive")
	}
}

// TestModelCallEventOnTruncation is the BUG-008 regression guard: a reply cut
// off by the token budget must still report its usage, because that is exactly
// the case where the hidden-token count explains the failure.
func TestModelCallEventOnTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"entity\":"},"finish_reason":"length"}],
			"usage":{"prompt_tokens":590,"completion_tokens":10,"total_tokens":4096}}`)
	}))
	defer srv.Close()

	var rec recorder
	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m"})
	p.SetObserver(rec.observe())

	_, err := p.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("a truncated reply must still be an error")
	}

	calls := rec.kind(EventModelCall)
	if len(calls) != 1 {
		t.Fatalf("expected 1 model_call event even on failure, got %d", len(calls))
	}
	ev := calls[0]
	if ev.FinishReason != "length" {
		t.Errorf("FinishReason = %q, want length", ev.FinishReason)
	}
	if ev.HiddenTokens != 3496 { // 4096 - 590 - 10
		t.Errorf("HiddenTokens = %d, want 3496", ev.HiddenTokens)
	}
	if ev.Outcome != OutcomeTransport || ev.Err == nil {
		t.Errorf("failed call must report an outcome and error: %s / %v", ev.Outcome, ev.Err)
	}
}

// TestModelCallEventEmittedOnEveryFailurePath: the event is emitted via defer,
// so even the earliest returns report.
func TestModelCallEventEmittedOnEveryFailurePath(t *testing.T) {
	t.Run("no baseURL configured", func(t *testing.T) {
		var rec recorder
		p := NewOpenAIProvider(ModelConfig{Model: "m"}) // no BaseURL at all
		p.SetObserver(rec.observe())

		if _, err := p.Complete(context.Background(), "s", "u"); err == nil {
			t.Fatal("expected an error")
		}
		if n := len(rec.kind(EventModelCall)); n != 1 {
			t.Errorf("got %d model_call events, want 1", n)
		}
	})

	t.Run("non-2xx from the endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `rate limited`)
		}))
		defer srv.Close()

		var rec recorder
		p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m"})
		p.SetObserver(rec.observe())

		if _, err := p.Complete(context.Background(), "s", "u"); err == nil {
			t.Fatal("expected an error")
		}
		calls := rec.kind(EventModelCall)
		if len(calls) != 1 {
			t.Fatalf("got %d model_call events, want 1", len(calls))
		}
		if calls[0].Outcome != OutcomeTransport {
			t.Errorf("outcome = %s, want transport_error", calls[0].Outcome)
		}
	})
}

// TestAnthropicProviderEmitsModelCallEvent: the second dialect reports the same
// three facts off a different wire shape (input/output tokens, stop_reason).
func TestAnthropicProviderEmitsModelCallEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":300,"output_tokens":40}}`)
	}))
	defer srv.Close()

	var rec recorder
	p := NewAnthropicProvider(ModelConfig{BaseURL: srv.URL, Model: "claude-opus-4-8", APIKeyEnv: ""})
	p.APIKey = "test-key"
	p.SetObserver(rec.observe())

	if _, err := p.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	calls := rec.kind(EventModelCall)
	if len(calls) != 1 {
		t.Fatalf("expected 1 model_call event, got %d", len(calls))
	}
	ev := calls[0]
	if ev.Provider != "anthropic" || ev.Model != "claude-opus-4-8" {
		t.Errorf("provider/model = %q/%q", ev.Provider, ev.Model)
	}
	if ev.PromptTokens != 300 || ev.CompletionTokens != 40 || ev.TotalTokens != 340 {
		t.Errorf("token mapping wrong: %+v", ev)
	}
	if ev.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q", ev.FinishReason)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Wiring
// ─────────────────────────────────────────────────────────────────────────────

// TestSetObserverWiresTheProvider: the whole reason SetObserver exists rather
// than assigning the field. Without the push-down, tokens and latency — the two
// facts worth watching — would be missing from an otherwise working setup.
func TestSetObserverWiresTheProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": canonicalAST},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var rec recorder
	e := NewWithProvider(engineTestConfig(t), NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m", Provider: "test"}))
	e.Now = func() time.Time { return fixedNow }
	e.SetObserver(rec.observe())

	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	// The full nesting: one model call inside one attempt inside one translate.
	if n := len(rec.kind(EventModelCall)); n != 1 {
		t.Errorf("got %d model_call events, want 1 — the observer did not reach the provider", n)
	}
	if n := len(rec.kind(EventAttempt)); n != 1 {
		t.Errorf("got %d attempt events, want 1", n)
	}
	if n := len(rec.kind(EventTranslate)); n != 1 {
		t.Errorf("got %d translate events, want 1", n)
	}
	// The model call must be reported before the attempt that contains it.
	seq := rec.kinds()
	if len(seq) != 3 || seq[0] != EventModelCall || seq[1] != EventAttempt || seq[2] != EventTranslate {
		t.Errorf("event order = %v, want [model_call attempt translate]", seq)
	}
}

// TestSetObserverOnAnUnreportingProviderStillEmits: a third-party ModelProvider
// (or a test stub) cannot report model calls, and that must degrade gracefully —
// you lose tokens and latency, never correctness.
func TestSetObserverOnAnUnreportingProviderStillEmits(t *testing.T) {
	var rec recorder
	e := newTestEngine(t, &StubProvider{Response: canonicalAST}) // no SetObserver method
	e.SetObserver(rec.observe())

	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if n := len(rec.kind(EventModelCall)); n != 0 {
		t.Errorf("a stub cannot report model calls, got %d", n)
	}
	if n := len(rec.kind(EventTranslate)); n != 1 {
		t.Errorf("engine events must still flow, got %d translate events", n)
	}
}

// TestFallbackChainReportsEveryAttemptedModel: when the primary fails and the
// secondary answers, both round trips must be visible — otherwise a chain that
// is silently always falling back looks identical to a healthy one.
func TestFallbackChainReportsEveryAttemptedModel(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `quota exceeded`)
	}))
	defer dead.Close()

	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer alive.Close()

	var rec recorder
	chain := ProvidersFrom(&Config{
		Model:  ModelConfig{BaseURL: dead.URL, Model: "primary", Provider: "p1"},
		Models: []ModelConfig{{BaseURL: alive.URL, Model: "secondary", Provider: "p2"}},
	})
	chain.(*FallbackProvider).SetObserver(rec.observe())

	if _, err := chain.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("chain should have fallen through to the secondary: %v", err)
	}

	calls := rec.kind(EventModelCall)
	if len(calls) != 2 {
		t.Fatalf("expected 2 model_call events (one per attempted model), got %d", len(calls))
	}
	if calls[0].Model != "primary" || calls[0].Outcome != OutcomeTransport {
		t.Errorf("first call = %s/%s, want primary/transport_error", calls[0].Model, calls[0].Outcome)
	}
	if calls[1].Model != "secondary" || calls[1].Outcome != OutcomeOK {
		t.Errorf("second call = %s/%s, want secondary/ok", calls[1].Model, calls[1].Outcome)
	}
}
