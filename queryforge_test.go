package queryforge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedProvider returns a different canned response on each call, so the
// repair loop can be exercised offline.
type scriptedProvider struct {
	responses []string
	calls     int
}

func (s *scriptedProvider) Complete(_ context.Context, _, _ string) (string, error) {
	i := s.calls
	if i >= len(s.responses) {
		i = len(s.responses) - 1 // repeat the last response if over-called
	}
	s.calls++
	return s.responses[i], nil
}

func engineTestConfig(t *testing.T) *Config { return mustParse(t, genConfigJSON) }

// newTestEngine wires an engine to a scripted/stub provider with a fixed clock.
func newTestEngine(t *testing.T, p ModelProvider) *Engine {
	e := NewWithProvider(engineTestConfig(t), p)
	e.Now = func() time.Time { return fixedNow }
	return e
}

// TestTranslateHappyPath: a valid AST from the model compiles to SQL with no
// repairs.
func TestTranslateHappyPath(t *testing.T) {
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})

	res, err := e.Translate(context.Background(), "delivered orders in the last 30 days", "sql")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.RepairAttempts != 0 {
		t.Errorf("expected 0 repairs, got %d", res.RepairAttempts)
	}
	if !strings.HasPrefix(res.Query.SQL, "SELECT * FROM orders WHERE") {
		t.Errorf("unexpected SQL: %s", res.Query.SQL)
	}
	if res.Explain == "" || res.Raw == "" {
		t.Errorf("explain/raw should be populated")
	}
}

// TestTranslateRepairRecovers: first model reply is invalid (unknown field),
// second is valid — the repair loop should recover and report one attempt.
func TestTranslateRepairRecovers(t *testing.T) {
	invalid := `{"entity":"Order","filter":{"type":"comparison","field":"stat","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	valid := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	e := newTestEngine(t, &scriptedProvider{responses: []string{invalid, valid}})

	res, err := e.Translate(context.Background(), "delivered orders", "sql")
	if err != nil {
		t.Fatalf("Translate should have recovered: %v", err)
	}
	if res.RepairAttempts != 1 {
		t.Errorf("expected 1 repair attempt, got %d", res.RepairAttempts)
	}
}

// TestTranslateFailsClosed: persistently invalid output must error, not guess.
func TestTranslateFailsClosed(t *testing.T) {
	invalid := `{"entity":"Order","filter":{"type":"comparison","field":"stat","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	e := newTestEngine(t, &StubProvider{Response: invalid})
	e.MaxRepairs = 1 // 2 attempts total

	_, err := e.Translate(context.Background(), "delivered orders", "sql")
	if err == nil || !strings.Contains(err.Error(), "failed validation") {
		t.Errorf("expected fail-closed error, got %v", err)
	}
}

// TestTranslateUnknownBackend rejects an unregistered backend.
func TestTranslateUnknownBackend(t *testing.T) {
	e := newTestEngine(t, &StubProvider{Response: canonicalAST})
	if _, err := e.Translate(context.Background(), "x", "cassandra"); err == nil {
		t.Error("expected unknown-backend error")
	}
}

// TestGenerateFromNoModelCall: the deterministic path must not touch the
// provider at all.
func TestGenerateFromNoModelCall(t *testing.T) {
	stub := &StubProvider{Response: canonicalAST}
	e := newTestEngine(t, stub)

	ast := canonicalQuery()
	res, err := e.GenerateFrom(ast, "mongo")
	if err != nil {
		t.Fatalf("GenerateFrom: %v", err)
	}
	if stub.Calls != 0 {
		t.Errorf("GenerateFrom must not call the model, but Calls=%d", stub.Calls)
	}
	if res.Doc == nil {
		t.Errorf("expected a mongo doc")
	}
}

// TestGenerateFromRejectsInvalid: an invalid AST is never compiled.
func TestGenerateFromRejectsInvalid(t *testing.T) {
	e := newTestEngine(t, &StubProvider{})
	bad := single(comp("nope", OpEquals, vStr("x")))
	if _, err := e.GenerateFrom(bad, "sql"); err == nil {
		t.Error("expected validation error")
	}
}

// TestGenerateFromFanOut: one AST compiles to multiple backends.
func TestGenerateFromFanOut(t *testing.T) {
	e := newTestEngine(t, &StubProvider{})
	ast := canonicalQuery()

	sql, err := e.GenerateFrom(ast, "sql")
	if err != nil {
		t.Fatalf("sql: %v", err)
	}
	mongo, err := e.GenerateFrom(ast, "mongo")
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	if sql.SQL == "" || mongo.Doc == nil {
		t.Errorf("fan-out produced empty results")
	}
}

// TestEngineValidateAndBackends covers the small helpers.
func TestEngineValidateAndBackends(t *testing.T) {
	e := newTestEngine(t, &StubProvider{})
	if err := e.Validate(canonicalQuery()); err != nil {
		t.Errorf("valid AST rejected: %v", err)
	}
	if got := strings.Join(e.Backends(), ","); got != "mongo,sql" {
		t.Errorf("backends = %s", got)
	}
}

// TestNewFromConfig checks the standard constructor wires a real provider.
func TestNewFromConfig(t *testing.T) {
	c := mustParse(t, genConfigJSON)
	e := New(c)
	if e.provider == nil || e.planner == nil {
		t.Error("New did not wire provider/planner")
	}
	if _, ok := e.provider.(*OpenAIProvider); !ok {
		t.Errorf("expected OpenAIProvider, got %T", e.provider)
	}
}

// --- BUG-005: parse failures are repairable, transport failures are not ---

// TestTranslateRepairsUnparseableOutput covers BUG-005. An unrecoverable first
// reply (an object that never closes — the truncated output actually observed
// live) must consume one repair attempt and then succeed, not abort the whole
// request.
func TestTranslateRepairsUnparseableOutput(t *testing.T) {
	p := &scriptedProvider{responses: []string{`{"entity":"Order","filter":{`, canonicalAST}}
	e := newTestEngine(t, p)

	res, err := e.Translate(context.Background(), "delivered orders", "sql")
	if err != nil {
		t.Fatalf("expected recovery from a malformed reply, got %v", err)
	}
	if p.calls != 2 {
		t.Errorf("expected 2 model calls (bad reply then repair), got %d", p.calls)
	}
	if res.RepairAttempts != 1 {
		t.Errorf("expected RepairAttempts=1, got %d", res.RepairAttempts)
	}
}

// TestRepairHintForParseFailureMentionsFormat checks the retry tells the model
// about output format rather than about validation, which would be misleading.
func TestRepairHintForParseFailureMentionsFormat(t *testing.T) {
	got := buildUserPrompt("orders", RepairHint{Kind: RepairParse, Message: "invalid character '}'"})
	if !strings.Contains(got, "could not be parsed as JSON") {
		t.Errorf("parse hint missing from prompt: %q", got)
	}
	if strings.Contains(got, "failed validation") {
		t.Errorf("parse hint should not claim a validation failure: %q", got)
	}
}

// TestTranslateDoesNotRetryTransportFailure pins the other half of BUG-005: a
// dead endpoint or rejected key must fail immediately rather than burn the
// repair budget (and the caller's quota) on calls that cannot succeed.
func TestTranslateDoesNotRetryTransportFailure(t *testing.T) {
	stub := &StubProvider{Err: errors.New("401 unauthorized")}
	e := newTestEngine(t, stub)

	if _, err := e.Translate(context.Background(), "delivered orders", "sql"); err == nil {
		t.Fatal("expected transport failure to propagate")
	}
	if stub.Calls != 1 {
		t.Errorf("transport failure must not be retried; got %d calls", stub.Calls)
	}
}

// TestTranslateGivesUpOnPersistentGarbage checks the repair budget is bounded:
// a model that never returns usable JSON fails closed with a message naming the
// output as the problem.
func TestTranslateGivesUpOnPersistentGarbage(t *testing.T) {
	p := &scriptedProvider{responses: []string{`not json at all`}}
	e := newTestEngine(t, p)

	_, err := e.Translate(context.Background(), "delivered orders", "sql")
	if err == nil {
		t.Fatal("expected failure after the repair budget is exhausted")
	}
	if !errors.Is(err, ErrModelOutput) {
		t.Errorf("expected ErrModelOutput, got %v", err)
	}
	if p.calls != e.MaxRepairs+1 {
		t.Errorf("expected %d attempts, got %d", e.MaxRepairs+1, p.calls)
	}
}

// --- BUG-007: an unavailable field is declined, never substituted ---

// TestParseASTDetectsRefusal checks the refusal marker becomes a typed error
// rather than decoding into an empty (unfiltered) Query.
func TestParseASTDetectsRefusal(t *testing.T) {
	c := mustParse(t, genConfigJSON)
	_, err := parseAST(`{"unsupported":"no field for shipping warehouse"}`, c)

	var unsupported *UnsupportedRequestError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected UnsupportedRequestError, got %v", err)
	}
	if !strings.Contains(unsupported.Reason, "shipping warehouse") {
		t.Errorf("reason not preserved: %q", unsupported.Reason)
	}
}

// TestTranslateSurfacesRefusal covers BUG-007 end to end: a refusal must reach
// the caller as a typed error, with no query compiled and no retry. Compiling
// an empty AST here would produce "return every row", the worst possible
// reading of "I cannot answer this".
func TestTranslateSurfacesRefusal(t *testing.T) {
	p := &scriptedProvider{responses: []string{`{"unsupported":"no field for courier"}`}}
	e := newTestEngine(t, p)

	res, err := e.Translate(context.Background(), "orders shipped by DHL", "sql")
	if res != nil {
		t.Errorf("expected no query for an unsupported request, got %+v", res.Query)
	}
	var unsupported *UnsupportedRequestError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected UnsupportedRequestError, got %v", err)
	}
	if p.calls != 1 {
		t.Errorf("a refusal is definitive and must not be retried; got %d calls", p.calls)
	}
}

// TestRefusalIsNotTaggedAsParseFailure guards the classification boundary: if a
// refusal were tagged ErrModelOutput the engine would retry it, re-asking a
// question the model has already answered.
func TestRefusalIsNotTaggedAsParseFailure(t *testing.T) {
	c := mustParse(t, genConfigJSON)
	pl := NewPlanner(c, &StubProvider{Response: `{"unsupported":"no such field"}`})

	_, _, err := pl.Plan(context.Background(), "x", RepairHint{})
	if errors.Is(err, ErrModelOutput) {
		t.Errorf("refusal must not be classified as unparseable output: %v", err)
	}
	if errors.Is(err, ErrModelTransport) {
		t.Errorf("refusal must not be classified as a transport failure: %v", err)
	}
}

// TestSystemPromptStatesRefusalContract checks the model is actually told how to
// decline; without this rule it substitutes a wrong field instead.
func TestSystemPromptStatesRefusalContract(t *testing.T) {
	c := mustParse(t, genConfigJSON)
	got := NewPlanner(c, &StubProvider{}).SystemPrompt(fixedNow)
	for _, want := range []string{"unsupported", "do not substitute a different field"} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}
