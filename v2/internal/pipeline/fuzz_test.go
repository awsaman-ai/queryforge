package pipeline

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/inputguard"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Phase 8, section 21's fuzz tests: the whole pipeline — extraction,
// retrieval, ranking, the read-only rule, binding, planning, compiling and
// the output guard — on arbitrary input. Retrieval is the deterministic
// hashing provider over the real commerce corpus: no semantics, but it
// matches configured words exactly, so fuzzed sentences that contain them
// reach every stage and compile.
//
// Invariants, for every input:
//   - no panic;
//   - a refusal carries one of the codes a SENTENCE can earn — never
//     INTERNAL_ERROR, CONFIG_ERROR or NOT_IMPLEMENTED, which would be bugs —
//     and no SQL;
//   - an answer is exactly one SELECT, ends in its only ;, and has one
//     parameter per placeholder. (Compile's output guard has already checked
//     the full lexicon; this restates the part a caller relies on.)
//
// `go test` runs the seeds; the gate runs `go test -fuzz=FuzzPipeline
// -fuzztime=…` for new inputs.

var (
	fuzzOnce sync.Once
	fuzzPipe *Pipeline
	fuzzErr  error
)

type indexRetriever struct {
	provider embedding.Provider
	index    *retrieval.Index
}

func (r indexRetriever) Retrieve(ctx context.Context, texts []string) ([][]retrieval.Match, error) {
	vs, err := r.provider.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	out := make([][]retrieval.Match, len(vs))
	for i, v := range vs {
		if out[i], err = r.index.Search(v, 10); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fuzzPipeline(t testing.TB) *Pipeline {
	fuzzOnce.Do(func() {
		cfg, err := config.Load("../../testdata/configs/commerce.json")
		if err != nil {
			fuzzErr = err
			return
		}
		reg := config.NewSchemaRegistry(cfg)
		provider := embedding.NewDeterministic(0)
		ix, err := retrieval.Build(context.Background(), provider, corpus.Build(reg), retrieval.StrategySurface)
		if err != nil {
			fuzzErr = err
			return
		}
		fuzzPipe, fuzzErr = New(reg, indexRetriever{provider, ix}, Options{CandidateDepth: 10})
	})
	if fuzzErr != nil {
		t.Fatal(fuzzErr)
	}
	return fuzzPipe
}

var sentenceCodes = map[qferr.Code]bool{
	qferr.CodeInvalidInput: true, qferr.CodeNoSemanticMatch: true, qferr.CodeLowConfidence: true,
	qferr.CodeAmbiguousMatch: true, qferr.CodeValueResolutionFailed: true, qferr.CodeUnsupportedQuery: true,
	qferr.CodeRelationshipNotFound: true, qferr.CodeSecurityRejected: true, qferr.CodeCompilationError: true,
}

var placeholder = regexp.MustCompile(`\$\d+`)

// fuzzSeeds are the brief's section 16 adversarial list, the Phase 7 Z
// cases, and shapes Phase 8 added: Unicode tricks, control characters,
// malformed numbers and dates, repetition.
var fuzzSeeds = []string{
	"Drop the customer table", "Delete all customers", "SELECT * FROM users; DROP TABLE users;",
	"' OR 1=1 --", "UNION SELECT password FROM users", "unknown_field", "unknown_operator",
	"customers named Robert'); DROP TABLE customers;--", "customers where name = 'x' OR 1=1",
	"customers from India", "customers who spent more than 50000", "top 5 customers by spending",
	"customers with country India and country India and country India",
	"custоmers from India", "customers​ from India", "‮customers from India",
	"ｃｕｓｔｏｍｅｒｓ", "customers\x00from India", "customers\tfrom\r\nIndia",
	"customers who spent more than 1e309", "customers who spent more than -0", "customers who spent more than 99999999999999999999999",
	"orders placed on 2024-13-45", "orders placed in the last 0 months", "orders placed in the last 999999999 years",
	"customers customers customers customers", "   ", "😀 customers 😀", "customers /* x */ from India",
	"show customers by update date", "orders by shipping date", "customers named %_\\",
	// Phase 8 gate (decision 022).
	"customers from India; select * from orders", "customers named Smith; --", "customers from India */",
	"customers who spent more than -5", "customers who spent more than .5", "customers who spent more than 4.35k",
	"customers who spent more than 1,5", "customers who spent more than 12x", "customers who spent more than one thousand two million",
	"show product discount price", "show product list price", "orders by return date",
}

func FuzzPipeline(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sentence string) {
		p := fuzzPipeline(t)
		res, err := p.Run(context.Background(), sentence, clock)
		// Decision 022 §3: statement or comment syntax never passes the input
		// stage, whatever surrounds it.
		if _, _, found := inputguard.Find(sentence); found && (err == nil || res.Stage != StageInput) {
			t.Fatalf("%q: holds statement syntax but reached %s (err %v)", sentence, res.Stage, err)
		}
		if err != nil {
			if code := qferr.CodeOf(err); !sentenceCodes[code] {
				t.Fatalf("%q: refused with %s, which only a bug produces: %v", sentence, code, err)
			}
			if res.Query.SQL != "" || len(res.Query.Params) != 0 {
				t.Fatalf("%q: a refusal carried a query", sentence)
			}
			return
		}
		sql := res.Query.SQL
		if res.Stage != StageDone || !strings.HasPrefix(sql, "SELECT ") || strings.Index(sql, ";") != len(sql)-1 {
			t.Fatalf("%q: not exactly one SELECT statement:\n%s", sentence, sql)
		}
		if got := len(placeholder.FindAllString(sql, -1)); got != len(res.Query.Params) {
			t.Fatalf("%q: %d placeholders, %d parameters", sentence, got, len(res.Query.Params))
		}
		// Decision 022 §2: every number parameter is, digit for digit, a
		// number extraction read from the sentence — none is rounded,
		// re-signed or made up on the way.
		read := map[string]bool{}
		for _, n := range res.Analysis.Numbers {
			if !n.Malformed {
				read[n.Value.String()] = true
			}
		}
		for _, pr := range res.Query.Params {
			if pr.Value.Kind == plan.NumberValue && !read[pr.Value.Number.String()] {
				t.Fatalf("%q: parameter %s is no number the sentence holds (%v)", sentence, pr.Value.Number, read)
			}
		}
	})
}

// The seeds, run as an ordinary test with the deterministic retriever: every
// one ends in an answer or an explicit refusal (which FuzzPipeline asserts)
// — and the write requests among them are refused as writes.
func TestFuzzSeedsWritesAreRefusedAsWrites(t *testing.T) {
	p := fuzzPipeline(t)
	// The two with ";" are refused at the input boundary since the gate
	// (decision 022 §3) — still SECURITY_REJECTED, one stage earlier.
	for _, s := range []string{"Drop the customer table", "Delete all customers", "SELECT * FROM users; DROP TABLE users;",
		"customers named Robert'); DROP TABLE customers;--"} {
		_, err := p.Run(context.Background(), s, clock)
		if qferr.CodeOf(err) != qferr.CodeSecurityRejected || err == nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}
