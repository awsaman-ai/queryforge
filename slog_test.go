package queryforge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers for the slog adapter.
// ─────────────────────────────────────────────────────────────────────────────

// logCapture is a logger writing JSON lines into a buffer, plus the helpers to
// read them back as maps. Asserting on decoded FIELDS rather than on the
// rendered line is deliberate: a test that string-matches a log line fails when
// someone reorders two attributes, which teaches people that logging tests are
// flaky and should be deleted.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
	log *slog.Logger
}

func newCapture(level slog.Level) *logCapture {
	c := &logCapture{}
	c.log = slog.New(slog.NewJSONHandler(&syncWriter{c: c}, &slog.HandlerOptions{Level: level}))
	return c
}

// syncWriter serializes writes, because several tests drive the Observer from
// more than one goroutine and slog's handler does not lock across our buffer.
type syncWriter struct{ c *logCapture }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}

// records decodes every line written so far.
func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	raw := c.buf.String()
	c.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// text returns everything written, for the canary tests that must search the
// whole stream rather than one field.
func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// only returns the single record whose operation matches, failing otherwise.
func only(t *testing.T, recs []map[string]any, operation string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, r := range recs {
		if r[logKeyOperation] == operation {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 %q record, got %d", operation, len(found))
	}
	return found[0]
}

// ─────────────────────────────────────────────────────────────────────────────
// Shape and levels
// ─────────────────────────────────────────────────────────────────────────────

// TestSlogObserverNilLoggerIsSilent: SlogObserver(nil) must disable logging, not
// hand back an Observer that panics on the first event. A caller wiring this up
// from a config where the logger is optional would otherwise crash on their
// first query rather than at startup.
func TestSlogObserverNilLoggerIsSilent(t *testing.T) {
	if o := SlogObserver(nil); o != nil {
		t.Fatal("SlogObserver(nil) should return a nil Observer")
	}
	// A nil Observer must be safe to emit through — that is the default path.
	SlogObserver(nil).emit(context.Background(), Event{Kind: EventTranslate})
}

// TestSlogLevelsMatchTheDocumentedContract enumerates every kind/outcome pair
// and pins its severity. This is the table the docs publish and that operators
// build alerts on, so a change here is a change to someone's pager.
func TestSlogLevelsMatchTheDocumentedContract(t *testing.T) {
	cases := []struct {
		event Event
		want  slog.Level
		why   string
	}{
		{Event{Kind: EventModelCall, Outcome: OutcomeOK}, slog.LevelDebug,
			"a successful model call is trace detail"},
		{Event{Kind: EventModelCall, Outcome: OutcomeTransport, Err: ErrModelTransport}, slog.LevelWarn,
			"one failed call may still be recovered by the next provider in a fallback chain"},

		{Event{Kind: EventAttempt, Outcome: OutcomeOK}, slog.LevelDebug,
			"a successful attempt is trace detail"},
		{Event{Kind: EventAttempt, Outcome: OutcomeRefusal}, slog.LevelInfo,
			"a refusal is the guard rail working, not a fault"},
		{Event{Kind: EventAttempt, Outcome: OutcomeValidation}, slog.LevelWarn,
			"a rejected attempt costs a model call but the budget continues"},
		{Event{Kind: EventAttempt, Outcome: OutcomeParseError}, slog.LevelWarn,
			"same: repairable inside the budget"},

		{Event{Kind: EventTranslate, Outcome: OutcomeOK}, slog.LevelInfo,
			"a completed translation is a lifecycle fact"},
		{Event{Kind: EventTranslate, Outcome: OutcomeRefusal}, slog.LevelInfo,
			"a refusal reaching the caller is still not an error"},
		{Event{Kind: EventTranslate, Outcome: OutcomeBudgetSpent}, slog.LevelError,
			"the caller is receiving an error"},
		{Event{Kind: EventTranslate, Outcome: OutcomeTransport}, slog.LevelError,
			"the caller is receiving an error"},
		{Event{Kind: EventTranslate, Outcome: OutcomeCallerError}, slog.LevelError,
			"the caller is receiving an error"},
		{Event{Kind: EventTranslate, Outcome: OutcomeGenerate}, slog.LevelError,
			"the caller is receiving an error"},
	}

	for _, tc := range cases {
		name := fmt.Sprintf("%s/%s", tc.event.Kind, tc.event.Outcome)
		t.Run(name, func(t *testing.T) {
			got, msg := logLevelFor(tc.event)
			if got != tc.want {
				t.Errorf("level = %v, want %v — %s", got, tc.want, tc.why)
			}
			if msg == "" {
				t.Error("every event needs a message; an empty one cannot be grouped")
			}
			if strings.ContainsAny(msg, "%\"") {
				t.Errorf("message %q looks interpolated; variable data belongs in attributes", msg)
			}
		})
	}
}

// TestSlogUnknownEventKindIsStillReported: an event kind added later, before
// logLevelFor learns about it, must produce a line rather than vanish. A
// silently dropped event is exactly the failure this file exists to prevent.
func TestSlogUnknownEventKindIsStillReported(t *testing.T) {
	c := newCapture(slog.LevelDebug)
	SlogObserver(c.log)(context.Background(), Event{Kind: EventKind("something_new"), Entity: "Order"})

	recs := c.records(t)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0][logKeyOperation] != "something_new" {
		t.Errorf("operation = %v, want the unrecognised kind carried through", recs[0][logKeyOperation])
	}
}

// TestSlogCarriesTheCrossLanguageFields pins the field names. They are a
// contract with the Python and Java SDKs and with anyone's saved dashboard
// query, so renaming one is a breaking change and should fail here first.
func TestSlogCarriesTheCrossLanguageFields(t *testing.T) {
	c := newCapture(slog.LevelDebug)
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(SlogObserver(c.log))

	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	rec := only(t, c.records(t), string(EventTranslate))
	for _, key := range []string{
		logKeyLibrary, logKeyLanguage, logKeyComponent, logKeyOperation,
		logKeyOutcome, logKeyEntity, logKeyBackend, logKeyAttempt, logKeyDuration,
		logKeyRepairAttempts,
	} {
		if _, ok := rec[key]; !ok {
			t.Errorf("translate record is missing %q; fields: %v", key, keysOf(rec))
		}
	}
	if rec[logKeyLibrary] != logLibraryName {
		t.Errorf("library = %v, want %q", rec[logKeyLibrary], logLibraryName)
	}
	if rec[logKeyLanguage] != "go" {
		t.Errorf("language = %v, want \"go\"", rec[logKeyLanguage])
	}
	if rec[logKeyComponent] != logComponentEngine {
		t.Errorf("component = %v, want %q", rec[logKeyComponent], logComponentEngine)
	}
}

// TestSlogErrorCodeAgreesWithClassify: the code on a log line and the code on
// the caller's error must be the same string, or a support conversation that
// starts "the log says MODEL_OUTPUT" cannot be matched to what the application
// caught.
func TestSlogErrorCodeAgreesWithClassify(t *testing.T) {
	cases := []struct {
		name     string
		provider ModelProvider
	}{
		{"transport", &erroringProvider{err: fmt.Errorf("dial: %w", ErrModelTransport)}},
		{"parse", &StubProvider{Response: unparseableReply}},
		{"validation", &StubProvider{Response: invalidAST}},
		{"refusal", &StubProvider{Response: refusalReply}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture(slog.LevelDebug)
			e := newTestEngine(t, tc.provider)
			e.SetObserver(SlogObserver(c.log))

			_, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
			if err == nil {
				t.Fatal("expected a failure")
			}
			want := string(Classify(err))

			rec := only(t, c.records(t), string(EventTranslate))
			if got := rec[logKeyErrorCode]; got != want {
				t.Errorf("log error_code = %v, caller's Classify = %q", got, want)
			}
			if rec[logKeyErrorType] == nil || rec[logKeyErrorType] == "" {
				t.Error("error_type must be set whenever an error is logged")
			}
			if rec[logKeyError] == nil || rec[logKeyError] == "" {
				t.Error("error message must be set whenever an error is logged")
			}
		})
	}
}

// TestSlogOneErrorPerFailedTranslation is the anti-noise rule from the design
// brief: a translation that burns its whole repair budget must produce the
// intermediate attempts at WARN and exactly ONE record at ERROR. Logging the
// same underlying failure at every layer is how an error stream stops being
// read.
func TestSlogOneErrorPerFailedTranslation(t *testing.T) {
	c := newCapture(slog.LevelDebug)
	e := newTestEngine(t, &StubProvider{Response: invalidAST}) // fails validation forever
	e.SetObserver(SlogObserver(c.log))

	if _, err := e.Translate(context.Background(), "delivered orders", "sql", nil); err == nil {
		t.Fatal("expected the repair budget to be exhausted")
	}

	var errors, warns int
	for _, rec := range c.records(t) {
		switch rec["level"] {
		case "ERROR":
			errors++
		case "WARN":
			warns++
		}
	}
	if errors != 1 {
		t.Errorf("got %d ERROR records, want exactly 1 (at the boundary)", errors)
	}
	if warns == 0 {
		t.Error("the rejected attempts should be visible at WARN")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Privacy canaries
//
// These are the tests that matter most. An Observer wired straight into a log
// aggregator is the EXPECTED configuration, so anything reachable from an Event
// is, in practice, going into a shared index. Each test plants a distinctive
// string somewhere a naive implementation would echo, and asserts it never
// appears — at DEBUG, which is the most verbose the library can be.
// ─────────────────────────────────────────────────────────────────────────────

// TestSlogNeverLogsTheQuestion: the natural-language question is user data. It
// may contain a name, an account number, or a medical detail, and it must not
// reach a log because someone turned QueryForge's logging on.
func TestSlogNeverLogsTheQuestion(t *testing.T) {
	const canary = "CANARY-QUESTION-8f3a1c"

	for _, tc := range []struct {
		name     string
		provider ModelProvider
	}{
		{"success", &StubProvider{Response: canonicalAST}},
		{"parse failure", &StubProvider{Response: unparseableReply}},
		{"validation failure", &StubProvider{Response: invalidAST}},
		{"transport failure", &erroringProvider{err: fmt.Errorf("dial: %w", ErrModelTransport)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture(slog.LevelDebug)
			e := newTestEngine(t, tc.provider)
			e.SetObserver(SlogObserver(c.log))

			_, _ = e.Translate(context.Background(), "orders for "+canary, "sql", nil)

			if strings.Contains(c.text(), canary) {
				t.Errorf("the question text reached the log:\n%s", c.text())
			}
		})
	}
}

// TestSlogNeverLogsScopeValues: scope values are tenant, user, subscription and
// enterprise ids — the most sensitive fields in the system and the ones most
// likely to be regulated. Only the KEYS may be logged.
func TestSlogNeverLogsScopeValues(t *testing.T) {
	const canary = "CANARY-TENANT-4b7e29"

	c := newCapture(slog.LevelDebug)
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	e.SetObserver(SlogObserver(c.log))

	if _, err := e.Translate(context.Background(), "delivered orders", "sql",
		Scope{"customerName": canary}); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if strings.Contains(c.text(), canary) {
		t.Errorf("a scope VALUE reached the log:\n%s", c.text())
	}
	// The key must be there — an audit trail that cannot say which filters were
	// forced onto a query is not an audit trail.
	rec := only(t, c.records(t), string(EventTranslate))
	keys, _ := rec[logKeyScopeKeys].([]any)
	if len(keys) != 1 || keys[0] != "customerName" {
		t.Errorf("scope_keys = %v, want [customerName]", rec[logKeyScopeKeys])
	}
}

// TestSlogRawIsDebugOnly: the model's verbatim reply echoes the question back in
// structured form and is the one Event field that can carry caller data. It is
// indispensable for diagnosing a parse failure and must not appear in a
// production log, so it is attached at DEBUG and nowhere else.
func TestSlogRawIsDebugOnly(t *testing.T) {
	const canary = "CANARY-RAW-1d9f04"
	reply := "not json at all, " + canary

	t.Run("absent at info", func(t *testing.T) {
		c := newCapture(slog.LevelInfo)
		e := newTestEngine(t, &StubProvider{Response: reply})
		e.SetObserver(SlogObserver(c.log))
		_, _ = e.Translate(context.Background(), "delivered orders", "sql", nil)

		if strings.Contains(c.text(), canary) {
			t.Errorf("the model's raw reply reached an INFO-level log:\n%s", c.text())
		}
	})

	t.Run("present at debug", func(t *testing.T) {
		c := newCapture(slog.LevelDebug)
		e := newTestEngine(t, &StubProvider{Response: reply})
		e.SetObserver(SlogObserver(c.log))
		_, _ = e.Translate(context.Background(), "delivered orders", "sql", nil)

		if !strings.Contains(c.text(), canary) {
			t.Error("at DEBUG the raw reply is the whole point; it should be present")
		}
	})
}

// TestSlogNeverLogsTheAPIKey: the key is read from the environment by the
// provider and never travels on an Event. This test proves it end to end rather
// than by reading the struct, because the risk is a future field — say, a
// verbatim HTTP request dump — quietly introducing one.
func TestSlogNeverLogsTheAPIKey(t *testing.T) {
	const canary = "CANARY-APIKEY-sk-77c1e2"

	c := newCapture(slog.LevelDebug)
	cfg := mustParse(t, genConfigJSON)
	provider := &OpenAIProvider{
		BaseURL:    "http://127.0.0.1:1", // refused immediately; no network needed
		Model:      "test",
		APIKey:     canary,
		ProviderID: "canary",
	}
	e := NewWithProvider(cfg, provider)
	e.SetObserver(SlogObserver(c.log))

	_, _ = e.Translate(context.Background(), "delivered orders", "sql", nil)

	if strings.Contains(c.text(), canary) {
		t.Errorf("the API key reached the log:\n%s", c.text())
	}
	// Sanity: the run really did fail through the provider, so the canary had a
	// chance to leak. Without this the test could pass by doing nothing.
	if !strings.Contains(c.text(), string(FailureModelTransport)) {
		t.Fatalf("expected a transport failure to have been logged; got:\n%s", c.text())
	}
}

// keysOf lists a record's field names for a readable failure message.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
