package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	qf "github.com/awsaman-ai/queryforge"
)

// testConfigJSON is the config every test in this file compiles against. It is
// written inline rather than read from examples/ so a change to a shipped
// example config cannot quietly rewrite what these tests assert.
//
// It deliberately covers the shapes the protocol has to carry: an enum with a
// domain, a non-returnable field (so projection has to become an allow-list), a
// physical-name mapping per backend, and both a SQL and a Mongo binding.
const testConfigJSON = `{
  "entity": "Order",
  "model": {"provider": "stub", "baseURL": "http://localhost", "model": "test"},
  "backends": {"sql": {"table": "orders"}, "mysql": {"table": "orders"}, "mongo": {"collection": "orders"}},
  "fields": [
    {"name": "status", "type": "enum", "values": ["NEW", "DELIVERED", "CANCELLED"], "indexed": true},
    {"name": "amount", "type": "number", "mapping": {"sql": "total_amount", "mysql": "total_amount"}},
    {"name": "customerName", "type": "string", "mapping": {"sql": "customer_name", "mysql": "customer_name"}},
    {"name": "internalNote", "type": "string", "returnable": false},
    {"name": "createdAt", "type": "date", "mapping": {"sql": "created_at", "mysql": "created_at"}}
  ]
}`

// validAST is a legal AST for testConfigJSON: status = DELIVERED.
const validAST = `{"version":"1.0","entity":"Order","filter":{"type":"comparison",` +
	`"field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`

// modelReply is what a well-behaved model returns for validAST. The planner
// tolerates a missing version, so this is the minimal shape.
const modelReply = `{"entity":"Order","filter":{"type":"comparison",` +
	`"field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`

// withProvider installs a stub model provider for the duration of one test, so
// the translate path can be exercised with no network and no API key. It
// restores the seam on cleanup, which matters because Go runs the tests in one
// process and a leaked stub would silently serve a later test.
func withProvider(t *testing.T, p qf.ModelProvider) {
	t.Helper()
	newEngine = func(c *qf.Config) *qf.Engine { return qf.NewWithProvider(c, p) }
	t.Cleanup(func() { newEngine = nil })
}

// request builds a Request with the test config already attached.
func request(t *testing.T, op Op, mutate func(*Request)) *Request {
	t.Helper()
	req := &Request{Op: op, Config: json.RawMessage(testConfigJSON)}
	if mutate != nil {
		mutate(req)
	}
	return req
}

// mustAST parses an AST literal, failing the test if it does not decode.
func mustAST(t *testing.T, s string) *qf.Query {
	t.Helper()
	var q qf.Query
	if err := json.Unmarshal([]byte(s), &q); err != nil {
		t.Fatalf("decode AST: %v", err)
	}
	return &q
}

// run dispatches a request with a background context and a real deadline, so a
// hung handler fails the test rather than the whole package timing out.
func dispatch(t *testing.T, req *Request) *Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return handle(ctx, req)
}

// wantError asserts a failed response carrying a specific code.
func wantError(t *testing.T, resp *Response, code Code) {
	t.Helper()
	if resp.Success {
		t.Fatalf("expected failure with code %s, got success: %+v", code, resp)
	}
	if resp.Code != code {
		t.Fatalf("expected code %s, got %s (message: %s)", code, resp.Code, resp.Message)
	}
	if resp.Message == "" {
		t.Errorf("code %s carried an empty message; every error must be readable", code)
	}
	if resp.Protocol != ProtocolVersion {
		t.Errorf("protocol = %q, want %q — an SDK checks this on every response", resp.Protocol, ProtocolVersion)
	}
}

// --- version op -------------------------------------------------------------

// TestVersionNeedsNoConfig: the handshake must answer even when the caller has
// no config at all, because an SDK performs it before it knows whether the
// user's config is usable.
func TestVersionNeedsNoConfig(t *testing.T) {
	resp := dispatch(t, &Request{Op: OpVersion})
	if !resp.Success {
		t.Fatalf("version failed: %+v", resp)
	}
	if resp.Protocol != ProtocolVersion {
		t.Errorf("protocol = %q, want %q", resp.Protocol, ProtocolVersion)
	}
	if resp.Version == "" {
		t.Error("version op returned no binary version")
	}
	// The backend list is what an SDK uses to reject a typo'd backend before
	// spending a model call, so it must be populated and must name the shipped
	// generators.
	if len(resp.Backends) == 0 {
		t.Fatal("version op returned no backends")
	}
	for _, want := range []string{"sql", "mysql", "mongo"} {
		if !contains(resp.Backends, want) {
			t.Errorf("backends %v is missing %q", resp.Backends, want)
		}
	}
}

// --- generate: the deterministic path ---------------------------------------

// TestGenerateSQL compiles a valid AST with no model call.
func TestGenerateSQL(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) {
		r.Backend = "sql"
		r.AST = mustAST(t, validAST)
	}))
	if !resp.Success {
		t.Fatalf("generate failed: %+v", resp)
	}
	if resp.Backend != "sql" {
		t.Errorf("backend = %q, want sql", resp.Backend)
	}
	if !strings.Contains(resp.SQL, "FROM orders") || !strings.Contains(resp.SQL, "WHERE") {
		t.Errorf("unexpected SQL: %s", resp.SQL)
	}
	// The value must be bound, never inlined — that is the injection guarantee
	// the whole library rests on, and it has to survive the trip through JSON.
	if len(resp.Args) != 1 || resp.Args[0] != "DELIVERED" {
		t.Errorf("args = %#v, want [DELIVERED] bound as a parameter", resp.Args)
	}
	if strings.Contains(resp.SQL, "DELIVERED") {
		t.Errorf("value was inlined into the SQL instead of bound: %s", resp.SQL)
	}
	// A non-returnable field must not appear in the projection even though the
	// AST requested no explicit select list.
	if strings.Contains(resp.SQL, "internal_note") || strings.Contains(resp.SQL, "internalNote") {
		t.Errorf("non-returnable field leaked into the projection: %s", resp.SQL)
	}
	if resp.Explain == "" {
		t.Error("generate returned no explanation")
	}
}

// TestGenerateMongo: the same AST against a document backend fills Doc, not SQL.
func TestGenerateMongo(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) {
		r.Backend = "mongo"
		r.AST = mustAST(t, validAST)
	}))
	if !resp.Success {
		t.Fatalf("generate failed: %+v", resp)
	}
	if resp.SQL != "" || len(resp.Args) != 0 {
		t.Errorf("mongo response populated the SQL fields: sql=%q args=%v", resp.SQL, resp.Args)
	}
	if resp.Doc == nil {
		t.Fatal("mongo response carried no doc")
	}
	// Round-trip through JSON, which is what an SDK actually receives — a Doc
	// that only looks right as a Go value would still be useless over the wire.
	raw, err := json.Marshal(resp.Doc)
	if err != nil {
		t.Fatalf("doc does not marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"collection":"orders"`) {
		t.Errorf("doc is missing the collection: %s", raw)
	}
}

// TestGenerateDefaultsToSQL: an omitted backend must not be an error. The doc's
// own examples name a dialect, but a caller compiling an AST need not.
func TestGenerateDefaultsToSQL(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) { r.AST = mustAST(t, validAST) }))
	if !resp.Success {
		t.Fatalf("generate failed: %+v", resp)
	}
	if resp.Backend != "sql" {
		t.Errorf("backend = %q, want the sql default", resp.Backend)
	}
}

// TestGenerateAppliesScope: caller-imposed filters are AND-ed in, reported back,
// and bound as parameters like any other value.
func TestGenerateAppliesScope(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) {
		r.Backend = "sql"
		r.AST = mustAST(t, validAST)
		r.Scope = qf.Scope{"customerName": "ACME"}
	}))
	if !resp.Success {
		t.Fatalf("generate failed: %+v", resp)
	}
	if len(resp.Scope) != 1 || resp.Scope[0].Field != "customerName" {
		t.Fatalf("scope not reported back: %+v", resp.Scope)
	}
	if !strings.Contains(resp.SQL, "customer_name") {
		t.Errorf("scope predicate missing from SQL: %s", resp.SQL)
	}
	if len(resp.Args) != 2 {
		t.Errorf("args = %#v, want the scope value and the filter value", resp.Args)
	}
	// Default ScopeInAST=false: the reported AST is the model's own, with no
	// scope spliced in, so it round-trips back through generate.
	if resp.AST.Filter.Type != qf.CondComparison {
		t.Errorf("scope leaked into the reported AST: %+v", resp.AST.Filter)
	}
}

// TestGenerateScopeInAST: the opt-in reports the effective AST instead.
func TestGenerateScopeInAST(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) {
		r.AST = mustAST(t, validAST)
		r.Scope = qf.Scope{"customerName": "ACME"}
		r.Options.ScopeInAST = true
	}))
	if !resp.Success {
		t.Fatalf("generate failed: %+v", resp)
	}
	if resp.AST.Filter.Type != qf.CondLogical || resp.AST.Filter.Op != qf.OpAND {
		t.Fatalf("expected the effective AST to be an AND of scope and filter, got %+v", resp.AST.Filter)
	}
}

// TestGenerateRejectsInvalidAST: a hallucinated field must fail closed, with
// the structured findings an SDK needs to point at the mistake.
func TestGenerateRejectsInvalidAST(t *testing.T) {
	// "amont" is a near-miss for the registered "amount", which is what makes the
	// suggestion list assertion below meaningful — a field name close to nothing
	// would legitimately produce no suggestions.
	bad := `{"version":"1.0","entity":"Order","filter":{"type":"comparison",` +
		`"field":"amont","operator":"gt","value":{"kind":"number","v":18}}}`
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) { r.AST = mustAST(t, bad) }))
	wantError(t, resp, CodeValidationFailed)

	if len(resp.Details) == 0 {
		t.Fatal("VALIDATION_FAILED carried no details; an SDK cannot name the bad field")
	}
	d := resp.Details[0]
	if d.Code != string(qf.CodeUnknownField) {
		t.Errorf("detail code = %q, want %q", d.Code, qf.CodeUnknownField)
	}
	if d.Field != "amont" {
		t.Errorf("detail field = %q, want amont", d.Field)
	}
	if d.Path == "" {
		t.Error("detail carried no path into the AST")
	}
	// The suggestion list is the difference between "unknown field" and "did you
	// mean amount?", and it must survive as data rather than only as prose.
	if len(d.Suggestions) == 0 {
		t.Error("unknown_field detail carried no suggestions")
	}
	// No query may be emitted alongside a failure — a caller that ignores
	// Success must not find something executable waiting for it.
	if resp.SQL != "" || resp.Doc != nil {
		t.Errorf("failed response carried a query: sql=%q doc=%v", resp.SQL, resp.Doc)
	}
}

// TestGenerateUnknownBackend names the registered ids so the caller can fix it.
func TestGenerateUnknownBackend(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) {
		r.Backend = "oracle"
		r.AST = mustAST(t, validAST)
	}))
	wantError(t, resp, CodeUnknownBackend)
	if !strings.Contains(resp.Message, "sql") {
		t.Errorf("message does not list the registered backends: %s", resp.Message)
	}
}

// TestGenerateRejectsBadScope: an undeclared scope field is the calling
// application's bug and must be told apart from a bad question.
func TestGenerateRejectsBadScope(t *testing.T) {
	resp := dispatch(t, request(t, OpGenerate, func(r *Request) {
		r.AST = mustAST(t, validAST)
		r.Scope = qf.Scope{"": "nothing"} // an empty key names no field
	}))
	wantError(t, resp, CodeInvalidScope)
}

// TestGenerateRequiresAST covers the caller-error path.
func TestGenerateRequiresAST(t *testing.T) {
	wantError(t, dispatch(t, request(t, OpGenerate, nil)), CodeInvalidRequest)
}

// --- validate op ------------------------------------------------------------

// TestValidateAcceptsLegalAST returns the explanation and no query.
func TestValidateAcceptsLegalAST(t *testing.T) {
	resp := dispatch(t, request(t, OpValidate, func(r *Request) { r.AST = mustAST(t, validAST) }))
	if !resp.Success {
		t.Fatalf("validate failed: %+v", resp)
	}
	if resp.SQL != "" || resp.Doc != nil {
		t.Errorf("validate compiled something; it must not: sql=%q doc=%v", resp.SQL, resp.Doc)
	}
	if resp.Explain == "" {
		t.Error("validate returned no explanation")
	}
}

// TestValidateReportsFindings: the sad path is an ordinary answer here, but is
// still reported as an error so an SDK's mapping stays uniform.
func TestValidateReportsFindings(t *testing.T) {
	bad := `{"version":"1.0","entity":"Wrong","filter":null}`
	resp := dispatch(t, request(t, OpValidate, func(r *Request) { r.AST = mustAST(t, bad) }))
	wantError(t, resp, CodeValidationFailed)
	if len(resp.Details) == 0 {
		t.Error("no findings reported")
	}
}

// TestValidateIgnoresBackend: validation is backend-independent, so a bogus
// backend id must not turn into an UNKNOWN_BACKEND error the caller cannot act
// on. Regression guard for the ordering inside handle().
func TestValidateIgnoresBackend(t *testing.T) {
	resp := dispatch(t, request(t, OpValidate, func(r *Request) {
		r.Backend = "oracle"
		r.AST = mustAST(t, validAST)
	}))
	if !resp.Success {
		t.Fatalf("validate should not consult the backend registry: %+v", resp)
	}
}

// --- translate: the model path ----------------------------------------------

// TestTranslateHappyPath runs the full pipeline against a stub model.
func TestTranslateHappyPath(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})

	resp := dispatch(t, request(t, OpTranslate, func(r *Request) {
		r.Backend = "mysql"
		r.Query = "delivered orders"
	}))
	if !resp.Success {
		t.Fatalf("translate failed: %+v", resp)
	}
	if !strings.Contains(resp.SQL, "FROM orders") {
		t.Errorf("unexpected SQL: %s", resp.SQL)
	}
	if resp.AST == nil || resp.Explain == "" {
		t.Error("translate returned no AST or explanation")
	}
	// Raw is off by default: it is derived from the user's question and the
	// response tends to end up in a log.
	if resp.Raw != "" {
		t.Errorf("raw model output was returned without includeRaw: %q", resp.Raw)
	}
}

// TestTranslateIncludeRaw opts into the model's verbatim reply.
func TestTranslateIncludeRaw(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})

	resp := dispatch(t, request(t, OpTranslate, func(r *Request) {
		r.Query = "delivered orders"
		r.Options.IncludeRaw = true
	}))
	if !resp.Success {
		t.Fatalf("translate failed: %+v", resp)
	}
	if resp.Raw == "" {
		t.Error("includeRaw was set but no raw output came back")
	}
}

// TestTranslateRequiresQuery rejects an empty question before spending a call.
func TestTranslateRequiresQuery(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})
	for _, q := range []string{"", "   ", "\n\t "} {
		resp := dispatch(t, request(t, OpTranslate, func(r *Request) { r.Query = q }))
		wantError(t, resp, CodeInvalidRequest)
	}
}

// TestTranslateRefusal: a model that declines maps to UNSUPPORTED_REQUEST, not
// to a generic failure — the message is meant for the end user.
func TestTranslateRefusal(t *testing.T) {
	withProvider(t, &qf.StubProvider{
		Response: `{"unsupported":"aggregation is not expressible in this AST"}`,
	})
	resp := dispatch(t, request(t, OpTranslate, func(r *Request) { r.Query = "average order value by month" }))
	wantError(t, resp, CodeUnsupportedRequest)
}

// TestTranslateUnparseableOutput: a model that never returns usable JSON
// exhausts the repair budget and reports MODEL_OUTPUT.
func TestTranslateUnparseableOutput(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: "I'm afraid I can't do that, Dave."})
	resp := dispatch(t, request(t, OpTranslate, func(r *Request) { r.Query = "delivered orders" }))
	wantError(t, resp, CodeModelOutput)
}

// TestTranslateTransportFailure: an unreachable model is distinguishable from a
// bad answer, because the fixes differ (check the key vs. rephrase).
func TestTranslateTransportFailure(t *testing.T) {
	withProvider(t, &qf.StubProvider{Err: errors.New("dial tcp: connection refused")})
	resp := dispatch(t, request(t, OpTranslate, func(r *Request) { r.Query = "delivered orders" }))
	wantError(t, resp, CodeModelTransport)
}

// TestTranslateValidationBudgetSpent: a model that keeps emitting a conforming-
// looking but illegal AST fails closed as VALIDATION_FAILED, with the findings
// intact through the budget-exhausted wrapper.
func TestTranslateValidationBudgetSpent(t *testing.T) {
	withProvider(t, &qf.StubProvider{
		Response: `{"entity":"Order","filter":{"type":"comparison","field":"status",` +
			`"operator":"equals","value":{"kind":"enum","v":"NOT_A_STATUS"}}}`,
	})
	resp := dispatch(t, request(t, OpTranslate, func(r *Request) { r.Query = "orders" }))
	wantError(t, resp, CodeValidationFailed)
	if len(resp.Details) == 0 {
		t.Error("findings were lost through the budget-exhausted wrapper")
	}
}

// TestTranslateMaxRepairsZero: maxRepairs is a pointer precisely so that 0
// means "one attempt, no repairs" rather than "unset". Assert the stub is
// called exactly once.
func TestTranslateMaxRepairsZero(t *testing.T) {
	stub := &qf.StubProvider{Response: "not json"}
	withProvider(t, stub)

	zero := 0
	resp := dispatch(t, request(t, OpTranslate, func(r *Request) {
		r.Query = "delivered orders"
		r.Options.MaxRepairs = &zero
	}))
	wantError(t, resp, CodeModelOutput)

	_, _, calls := stub.Snapshot()
	if calls != 1 {
		t.Errorf("model was called %d times with maxRepairs=0, want 1", calls)
	}
}

// TestTranslateTimeout: an expired deadline is reported as TIMEOUT rather than
// as a transport failure, so the caller extends the deadline instead of
// hunting for a network problem.
func TestTranslateTimeout(t *testing.T) {
	withProvider(t, blockingProvider{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	resp := handle(ctx, request(t, OpTranslate, func(r *Request) { r.Query = "delivered orders" }))
	wantError(t, resp, CodeTimeout)
}

// blockingProvider never answers until the context is done, standing in for a
// hung endpoint.
type blockingProvider struct{}

func (blockingProvider) Complete(ctx context.Context, _, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// --- request-level errors ---------------------------------------------------

// TestMissingOp and friends: the caller-error paths must each name what is
// wrong, because they are what an SDK author sees while wiring things up.
func TestMissingOp(t *testing.T) {
	wantError(t, dispatch(t, &Request{}), CodeInvalidRequest)
}

func TestUnknownOp(t *testing.T) {
	resp := dispatch(t, &Request{Op: "delete"})
	wantError(t, resp, CodeUnknownOp)
	if !strings.Contains(resp.Message, "translate") {
		t.Errorf("message does not list the supported ops: %s", resp.Message)
	}
}

func TestMissingConfig(t *testing.T) {
	wantError(t, dispatch(t, &Request{Op: OpGenerate, AST: mustAST(t, validAST)}), CodeInvalidRequest)
}

// TestInvalidConfig covers both a JSON syntax error and a config that parses
// but breaks the engine's own structural rules.
func TestInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"not json":            `{"entity": "Order",`,
		"enum with no values": `{"entity":"Order","fields":[{"name":"status","type":"enum"}]}`,
		"api key pasted in":   `{"entity":"Order","model":{"apiKeyEnv":"sk-ant-secret"},"fields":[{"name":"a","type":"string"}]}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			resp := dispatch(t, &Request{Op: OpGenerate, Config: json.RawMessage(cfg), AST: mustAST(t, validAST)})
			wantError(t, resp, CodeInvalidConfig)
			// A config error must never echo a pasted secret back into whatever log
			// the response lands in.
			if strings.Contains(resp.Message, "sk-ant-secret") {
				t.Errorf("config error echoed the pasted secret: %s", resp.Message)
			}
		})
	}
}

// contains reports whether s holds v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
