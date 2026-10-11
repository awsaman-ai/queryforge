package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Decision 022 §3: a request is one query. SQL statement and comment syntax
// refuses it at the INPUT stage, so it produces no query and nothing after the
// boundary runs — not extraction, not retrieval (the model), not binding, not
// the compiler.

// countingRetriever records every call; the boundary tests require none.
type countingRetriever struct {
	mu    sync.Mutex
	calls int
}

func (c *countingRetriever) Retrieve(_ context.Context, texts []string) ([][]retrieval.Match, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return make([][]retrieval.Match, len(texts)), nil
}

// The recorded cases (Phase 7's Z04, Z05; Phase 8's W16, I01, I04, I07–I09)
// and the forms the rule is written for.
var statementSyntax = []string{
	"customers from India; select * from orders", // I07: two requests merged
	"customers from India; drop table customers", // W16
	"SELECT * FROM users; DROP TABLE users;",     // I01
	"customers named Robert'); DROP TABLE customers;--",
	"customers/**/from/**/India", // I08: compiled before the gate
	"customers named Smith; --",  // I09: compiled before the gate
	"' OR 1=1 --",                // Z04, I02
	"customers from India;",      // a lone trailing separator
	"customers from India -- and country US",
	"customers from India */",
	"customers from India" + string(rune(0xFF1B)) + " orders",
	"customers from India -" + string(rune(0x200B)) + "- orders",
}

func TestStatementSyntaxNeverReachesAStage(t *testing.T) {
	reg := registry(t, "commerce.json")
	for _, in := range statementSyntax {
		r := &countingRetriever{}
		p, err := New(reg, r, Options{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.Run(context.Background(), in, clock)
		if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected || res.Stage != StageInput {
			t.Errorf("%q: stage %s err %v", in, res.Stage, err)
			continue
		}
		if !strings.Contains(err.Error(), "SQL statement syntax") {
			t.Errorf("%q: the refusal does not state its reason: %v", in, err)
		}
		// No query, and no stage after the boundary ran.
		if res.Query.SQL != "" || len(res.Query.Params) != 0 || len(res.Plan.Select) != 0 ||
			res.Binding.Root != "" || len(res.Analysis.Tokens) != 0 || len(res.Ranking.Spans) != 0 {
			t.Errorf("%q: a refused request carries later-stage output: %+v", in, res.Query)
		}
		if r.calls != 0 {
			t.Errorf("%q: the retriever (the model) ran %d times for a refused request", in, r.calls)
		}
		// Telemetry names the stage and code, nothing else.
		if res.Stats.Stage != StageInput || res.Stats.ErrorCode != qferr.CodeSecurityRejected || res.Stats.QueryLength != 0 {
			t.Errorf("%q: stats %+v", in, res.Stats)
		}

		// The diagnostic path (--rank, --bind, --plan) is refused the same way.
		if _, _, err := p.Rank(context.Background(), in); err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected {
			t.Errorf("%q: Rank did not refuse: %v", in, err)
		}
		if r.calls != 0 {
			t.Errorf("%q: Rank reached the retriever", in)
		}
	}
}

// The same sentences without the syntax are ordinary requests: the rule is
// about the delimiters, not about the words around them.
func TestTheSameRequestWithoutTheSyntaxIsRead(t *testing.T) {
	res, err := run(t, "customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia})
	if err != nil || res.Stage != StageDone {
		t.Fatalf("stage %s err %v", res.Stage, err)
	}
	// A hyphen, a slash and a star are not delimiters.
	reg := registry(t, "commerce.json")
	for _, in := range []string{"top-5 customers", "customers - India", "orders/customers", "5 * 3 customers"} {
		p, _ := New(reg, &countingRetriever{}, Options{})
		if _, err := p.Run(context.Background(), in, clock); err != nil && strings.Contains(err.Error(), "SQL statement syntax") {
			t.Errorf("%q was refused as statement syntax", in)
		}
	}
}

// Order at the input stage: the length limit first (a request past it is not
// scanned), then the boundary.
func TestInputLengthIsCheckedBeforeTheBoundary(t *testing.T) {
	reg := registry(t, "commerce.json")
	p, _ := New(reg, &countingRetriever{}, Options{})
	long := strings.Repeat("customers ", 100) + ";"
	_, err := p.Run(context.Background(), long, clock)
	if qferr.CodeOf(err) != qferr.CodeInvalidInput || err == nil {
		t.Errorf("err = %v, want the length refusal", err)
	}
}
