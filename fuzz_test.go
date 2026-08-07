package queryforge

// Fuzz targets for the three functions that consume input the library does not
// control: SECURITY-T.md pre-release checklist item 8.
//
// Each of the three sits on a trust boundary:
//
//	parseAST      the model's reply. Semi-trusted at best — it is influenced by
//	              a fully hostile sentence, and a compromised or merely broken
//	              provider can return anything at all.
//	ParseConfig   a file on disk. Trusted in the threat model, but it is the
//	              input a developer edits by hand, and a malformed one must fail
//	              at load with an error rather than at query time with a panic.
//	Validate      the AST. This is THE control: every guarantee the library makes
//	              about model output is a thing Validate rejects. A panic here is
//	              not a crash, it is a bypass, because a panicking validator
//	              never returns "invalid".
//
// WHAT THESE ASSERT, precisely: that the function returns. Not that it returns
// the right answer — an oracle for "is this the correct AST" is the rest of the
// suite's job, and a fuzzer cannot supply one. A panic on attacker-influenced
// input is a denial of service in a library whose caller is usually an HTTP
// handler, and for Validate it is worse than that, so "it returned at all" is
// the property worth a fuzzer.
//
// The two invariants below are the exceptions, and are the reason these targets
// earn their keep beyond crash-hunting:
//
//	parseAST  must never return a non-nil Query that is structurally empty
//	          (S-2 — that combination IS the fail-open bug).
//	Validate  must never accept an AST that names a field the config does not
//	          declare (S-7 — the containment claim, checked against arbitrary
//	          input rather than against the 19 cases someone thought of).
//
// Run them with:
//
//	go test -run=Fuzz -fuzz=FuzzParseAST -fuzztime=60s
//
// They are ordinary unit tests over the seed corpus in a normal `go test` run,
// which is what makes them useful in CI without a fuzzing budget.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzParseAST throws arbitrary bytes at the model-reply parser.
//
// The seeds are the reply shapes that have actually caused defects: the
// double-brace Gemini output behind BUG-008, the truncated object that must not
// be auto-closed, the fenced reply, the refusal, and the empty object from S-2.
func FuzzParseAST(f *testing.F) {
	seeds := []string{
		``,
		`{}`,
		`{"entity":"Order"}`,
		`{"unsupported":"no such field"}`,
		`{"unsupported":""}`,
		"```json\n{\"entity\":\"Order\"}\n```",
		`{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`,
		`{"entity":"Order"}}`,                       // the spurious trailing brace
		`{"entity":"Order","filter":{"type":"com`,   // truncated mid-object
		`{"entity":"Order","limit":99999999999999}`, // number wider than an int
		`{"filter":{"type":"logical","op":"AND","children":[{"type":"logical","op":"AND","children":[]}]}}`,
		`{"select":["a","a","a"]}`,
		`{"filter":{"type":"comparison","field":"x","operator":"equals","value":{"kind":"relative_date","v":{"unit":"days","amount":-9223372036854775808}}}}`,
		"{\"entity\":\"Order\",\"note\":\"}\"}", // a brace inside a string literal
		`[1,2,3]`,
		`null`,
		`not json at all`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	c := mustParseFuzzConfig(f)

	f.Fuzz(func(t *testing.T, raw string) {
		q, err := parseAST(raw, c)

		// A parser that returns neither a query nor an error leaves the caller
		// with nothing to branch on.
		if err == nil && q == nil {
			t.Fatalf("parseAST(%q) returned nil, nil", raw)
		}
		if err != nil {
			if q != nil {
				t.Fatalf("parseAST(%q) returned both a query and an error %v", raw, err)
			}
			return
		}

		// S-2 as an invariant rather than a list of cases: no input may produce
		// a query that says nothing. That combination is exactly what compiled
		// to an unfiltered read and was reported as a success.
		if isStructurallyEmpty(q) {
			t.Fatalf("parseAST(%q) accepted a structurally empty query", raw)
		}
		// The two fields parseAST is responsible for defaulting must be set, or
		// the entity check in Validate has nothing to compare against.
		if q.Version == "" || q.Entity == "" {
			t.Fatalf("parseAST(%q) left version=%q entity=%q unset", raw, q.Version, q.Entity)
		}
	})
}

// FuzzParseConfig throws arbitrary bytes at the config loader.
//
// A config is trusted input, so this is not hunting for an injection — it is
// hunting for the panic that turns a typo in a JSON file into a crash at
// startup, and for a config that loads successfully but is internally
// inconsistent, which is the state finalize() exists to make impossible.
func FuzzParseConfig(f *testing.F) {
	seeds := []string{
		``,
		`{}`,
		`{"entity":"Order"}`,
		secConfigJSON,
		`{"entity":"Order","fields":[{"name":"a","type":"enum"}]}`,                                // enum with no values
		`{"entity":"Order","fields":[{"name":"a","type":"nosuchtype"}]}`,                          // unknown type
		`{"entity":"Order","fields":[{"name":"a","type":"string"},{"name":"a","type":"number"}]}`, // duplicate
		`{"entity":"Order","fields":[{"name":"a","type":"string","operators":["nosuchop"]}]}`,
		`{"entity":"Order","fields":[{"name":"a","type":"string","mapping":{"mongo":"$bad"}}]}`,
		`{"entity":"Order","fields":[{"name":"a","type":"string","mapping":{"sql":"a; DROP TABLE x"}}]}`,
		`{"entity":"Order","fields":[{"name":"a","type":"number","valueCase":"upper"}]}`,
		`{"entity":"Order","policy":{"maxNestingDepth":-1,"maxFilterNodes":-1}}`,
		`{"entity":"Order","typo":true}`, // DisallowUnknownFields must catch this
		`{"entity":"Order","defaults":{"limit":100,"maxLimit":10}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		c, err := ParseConfig([]byte(raw))
		if err != nil {
			if c != nil {
				t.Fatalf("ParseConfig returned a config alongside error %v", err)
			}
			return
		}
		if c == nil {
			t.Fatal("ParseConfig returned nil, nil")
		}

		// A config that loaded is a config the rest of the library will trust.
		// Re-check the two properties everything downstream assumes, because a
		// loader that accepts these has moved the failure to a generator, where
		// it becomes a broken query rather than a startup error.
		if c.Entity == "" {
			t.Fatal("ParseConfig accepted a config with no entity")
		}
		seen := map[string]bool{}
		for i := range c.Fields {
			name := c.Fields[i].Name
			if name == "" {
				t.Fatal("ParseConfig accepted a field with no name")
			}
			if seen[name] {
				t.Fatalf("ParseConfig accepted a duplicate field %q", name)
			}
			seen[name] = true
		}

		// A loaded config must survive being used. Validate and both generators
		// are run against a trivial query, because "the config parsed" and "the
		// config can compile anything" have been different things before.
		q := NewQuery(c.Entity)
		_ = Validate(q, c)
		for _, backend := range DefaultRegistry().Backends() {
			g, ok := DefaultRegistry().Get(backend)
			if !ok {
				continue
			}
			_, _ = g.Generate(q, c, GenOptions{Now: fixedNow})
		}
	})
}

// FuzzValidate throws arbitrary AST JSON at the validator.
//
// This is the important one. Validate is the single control standing between
// semi-trusted model output and a compiled query, so a panic here is not a
// crash but a bypass: a validator that panics never returns "invalid", and a
// caller recovering the panic at the HTTP layer learns nothing about what the
// AST claimed.
func FuzzValidate(f *testing.F) {
	seeds := []string{
		`{}`,
		`{"entity":"Order"}`,
		`{"entity":"Wrong"}`,
		`{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`,
		`{"entity":"Order","filter":{"type":"comparison","field":"nosuch","operator":"equals","value":{"kind":"string","v":"x"}}}`,
		`{"entity":"Order","filter":{"type":"comparison","field":"internalNote","operator":"equals","value":{"kind":"string","v":"x"}}}`,
		`{"entity":"Order","filter":{"type":"logical","op":"NOT","children":[]}}`,
		`{"entity":"Order","filter":{"type":"logical","op":"AND"}}`,
		`{"entity":"Order","filter":{"type":"nosuchtype"}}`,
		`{"entity":"Order","filter":{"type":"comparison","field":"customerName","operator":"regex","value":{"kind":"string","v":"(a+)+$"}}}`,
		`{"entity":"Order","filter":{"type":"comparison","field":"amount","operator":"between","value":{"kind":"array","v":[1]}}}`,
		`{"entity":"Order","select":["ssn"]}`,
		`{"entity":"Order","select":["status","status"]}`,
		`{"entity":"Order","sort":[{"field":"nosuch","dir":"asc"}]}`,
		`{"entity":"Order","limit":-1}`,
		`{"entity":"Order","limit":999999999}`,
		`{"entity":"Order","version":"99.0"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	c := mustParseFuzzConfig(f)

	f.Fuzz(func(t *testing.T, raw string) {
		// Decode the way the engine does. A payload that is not an AST at all is
		// not this function's subject — parseAST owns that — so skip rather than
		// fail, which keeps the corpus pointed at ASTs.
		var q Query
		if err := json.Unmarshal([]byte(raw), &q); err != nil {
			t.Skip()
		}

		err := Validate(&q, c) // must return, whatever it was handed
		if err != nil {
			return
		}

		// It said the AST is legal. Two things must then be true, and both are
		// containment claims from S-7 rather than restatements of the validator.

		// 1. Every field the AST names is declared. This is what makes "an
		//    unregistered field is rejected" a guarantee instead of an
		//    observation about the cases someone wrote tests for.
		for _, name := range astFieldNames(&q) {
			if _, ok := c.FieldByName(name); !ok {
				t.Fatalf("Validate accepted an AST naming the undeclared field %q: %s", name, raw)
			}
		}

		// 2. A valid AST compiles. Validate's contract to the generators is that
		//    they may assume a validated tree, so anything it accepts and they
		//    reject is a gap between the two — which is how QF-T-006 (generators
		//    panic on an unvalidated AST) happened.
		for _, backend := range []string{"sql", "mongo"} {
			g, ok := DefaultRegistry().Get(backend)
			if !ok {
				continue
			}
			if _, gerr := g.Generate(&q, c, GenOptions{Now: fixedNow}); gerr != nil {
				// A generator may legitimately refuse an empty projection, which
				// is a config property rather than an AST one.
				if strings.Contains(gerr.Error(), "projection") {
					continue
				}
				t.Fatalf("Validate accepted an AST that %s could not compile: %v\nAST: %s", backend, gerr, raw)
			}
		}
	})
}

// FuzzUnsafeRegexShape throws arbitrary patterns at the ReDoS screen (S-3).
//
// The screen runs on every regex predicate the validator sees, so it is on the
// hot path for input the model chooses. It must terminate and must not panic on
// a malformed pattern — unbalanced parens, a truncated character class, a
// trailing backslash — because those reach it routinely: the check deliberately
// runs BEFORE anything compiles the pattern, so it is the first code to touch a
// string no parser has vetted.
func FuzzUnsafeRegexShape(f *testing.F) {
	seeds := []string{
		``, `(a+)+`, `(a+)+$`, `(x+x+)+y`, `^ACME-\d+$`, `(cat|dog)*`,
		`[+*]+`, `\(a+\)+`, `(a{2,3})+`, `(a{2,})+`, `(?:a+)+`,
		`(a+`, `a)+`, `[a-`, `\`, `((((((((((a+))))))))))+`,
		`{2,}`, `a{`, `a{,}`, `a{1,2,3}`, `[]]+`, `[^]]+`,
		strings.Repeat("(", 100) + "a+" + strings.Repeat(")", 100) + "+",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, pat string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			frag, unsafe := unsafeRegexShape(pat)
			// The fragment is quoted back to the model in a repair hint, so it
			// has to be a real slice of the pattern rather than an index slip.
			if unsafe && !strings.Contains(pat, frag) {
				t.Errorf("reported fragment %q is not a substring of %q", frag, pat)
			}
			if unsafe && frag == "" {
				t.Errorf("pattern %q flagged with an empty fragment", pat)
			}
		}()

		// The screen is a single left-to-right pass and cannot loop, but it
		// exists to prevent an unbounded evaluation — so a version of it that
		// looped would be a particularly poor bug to ship.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("unsafeRegexShape did not terminate on %q", pat)
		}
	})
}

// mustParseFuzzConfig builds the shared config for the AST-facing targets.
// Takes *testing.F because a seed-corpus failure is a setup failure, not a
// finding.
func mustParseFuzzConfig(f *testing.F) *Config {
	f.Helper()
	c, err := ParseConfig([]byte(secConfigJSON))
	if err != nil {
		f.Fatalf("fuzz config does not parse: %v", err)
	}
	return c
}

// astFieldNames collects every field name an AST mentions — in the filter tree,
// the projection and the sort list — so the containment invariant can be checked
// without duplicating the validator's traversal logic in an assertion.
func astFieldNames(q *Query) []string {
	var out []string
	var walkCond func(*Condition, int)
	walkCond = func(c *Condition, depth int) {
		// Depth-guarded: the input is arbitrary JSON, and encoding/json will
		// happily build a tree deep enough to overflow the stack of a recursive
		// helper that trusts it.
		if c == nil || depth > 200 {
			return
		}
		if c.Field != "" {
			out = append(out, c.Field)
		}
		for _, kid := range c.Children {
			walkCond(kid, depth+1)
		}
	}
	walkCond(q.Filter, 0)
	out = append(out, q.Select...)
	for _, s := range q.Sort {
		out = append(out, s.Field)
	}
	return out
}
