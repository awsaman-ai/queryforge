package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Phase 8, section 16.7: the limits the pipeline owns — phrases, candidates,
// planning time. Every limit is tested in both directions: AT the limit the
// request is answered, one past it the request is refused with the stated
// code, at the stated stage, and with a message naming the real reason.
// Decision 021 records what each limit counts.

// registryWith loads commerce.json and applies one change to its security
// policy, after Load's defaults, so the rest of the config is untouched.
func registryWith(t *testing.T, change func(*config.SecurityPolicy)) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", "commerce.json"))
	if err != nil {
		t.Fatal(err)
	}
	change(&cfg.Security)
	return config.NewSchemaRegistry(cfg)
}

var example1 = map[string][]hit{"customers": hCustomers, "india": hIndia}

// "Show customers from India" yields exactly this many candidate spans. The
// limits below are set relative to it, so the test states its own premise.
func example1Spans(t *testing.T) int {
	t.Helper()
	return len(nlp.Analyze("Show customers from India", nlp.DefaultOptions).Spans)
}

// maxPhrases is read from the config, not from nlp.DefaultOptions: at the
// limit the sentence compiles, one under it the request is refused before
// retrieval — the retriever is never called.
func TestMaxPhrasesComesFromTheConfig(t *testing.T) {
	n := example1Spans(t)
	for _, c := range []struct {
		limit  int
		refuse bool
	}{{n, false}, {n - 1, true}} {
		reg := registryWith(t, func(s *config.SecurityPolicy) { s.MaxPhrases = c.limit })
		s := newScripted(reg, example1)
		p, err := New(reg, s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.Run(context.Background(), "Show customers from India", clock)
		if !c.refuse {
			if err != nil || res.Stage != StageDone {
				t.Errorf("maxPhrases %d (= the sentence's %d spans): stage %s err %v", c.limit, n, res.Stage, err)
			}
			continue
		}
		if err == nil || qferr.CodeOf(err) != qferr.CodeInvalidInput || res.Stage != StageExtract {
			t.Fatalf("maxPhrases %d < %d spans: stage %s err %v", c.limit, n, res.Stage, err)
		}
		if !strings.Contains(err.Error(), "candidate spans, over the limit of") {
			t.Errorf("message does not state the reason: %v", err)
		}
		if len(s.used) != 0 {
			t.Errorf("the retriever ran for a request refused before retrieval: %v", s.used)
		}
	}
}

// maxInputLength counts CHARACTERS (runes), not bytes: at the limit the
// request is read, one character past it the request is refused at the input
// stage — never truncated — and nothing after the input stage runs. Retained
// as a resource-bound test at the Phase 8 gate (decision 022 §4).
func TestMaxInputLengthInBothDirections(t *testing.T) {
	const limit = 40
	reg := registryWith(t, func(s *config.SecurityPolicy) { s.MaxInputLength = limit })
	base := "Show customers from India"
	// Pad with a multi-byte letter, so a byte count would disagree.
	pad := func(n int) string { return base + " " + strings.Repeat(string(rune(0x00E9)), n-len(base)-1) }
	at, over := pad(limit), pad(limit+1)
	if len([]rune(at)) != limit || len(at) == limit {
		t.Fatalf("premise: %d runes, %d bytes", len([]rune(at)), len(at))
	}
	for _, c := range []struct {
		in     string
		refuse bool
	}{{at, false}, {over, true}} {
		r := &countingRetriever{}
		p, err := New(reg, r, Options{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.Run(context.Background(), c.in, clock)
		if !c.refuse {
			if res.Stage == StageInput {
				t.Errorf("at the limit (%d characters): refused at input: %v", limit, err)
			}
			continue
		}
		if err == nil || qferr.CodeOf(err) != qferr.CodeInvalidInput || res.Stage != StageInput ||
			!strings.Contains(err.Error(), "characters, over the limit of 40") {
			t.Errorf("one past the limit: stage %s err %v", res.Stage, err)
		}
		if r.calls != 0 || len(res.Analysis.Tokens) != 0 {
			t.Errorf("a request over the limit was read (retriever calls %d)", r.calls)
		}
	}
}

// The raised default is the limit Phase 3 already applied, which is why
// enforcing the config value changed no earlier result (decision 021). If one
// moves without the other, that claim needs re-measuring.
func TestDefaultPhraseLimitIsPhase3sLimit(t *testing.T) {
	if config.DefaultSecurityPolicy.MaxPhrases != nlp.DefaultOptions.MaxSpans {
		t.Fatalf("security.maxPhrases default %d, Phase 3's span limit %d",
			config.DefaultSecurityPolicy.MaxPhrases, nlp.DefaultOptions.MaxSpans)
	}
}

// A declared retrieval depth over maxCandidates is a startup error, not a
// per-request one; at the limit it is accepted.
func TestCandidateDepthIsCheckedAtStartup(t *testing.T) {
	reg := registryWith(t, func(s *config.SecurityPolicy) { s.MaxCandidates = 10 })
	for _, c := range []struct {
		depth int
		code  qferr.Code // "" = accepted
	}{{0, ""}, {10, ""}, {11, qferr.CodeConfigError}, {-1, qferr.CodeConfigError}} {
		_, err := New(reg, newScripted(reg, nil), Options{CandidateDepth: c.depth})
		if c.code == "" {
			if err != nil {
				t.Errorf("depth %d: %v", c.depth, err)
			}
			continue
		}
		if err == nil || qferr.CodeOf(err) != c.code {
			t.Errorf("depth %d: err %v, want %s", c.depth, err, c.code)
		}
	}
}

// overfull returns more candidates for one span than it would declare.
type overfull struct{ inner *scripted }

func (o overfull) Retrieve(ctx context.Context, texts []string) ([][]retrieval.Match, error) {
	out, err := o.inner.Retrieve(ctx, texts)
	for i := range out {
		for len(out[i]) > 0 && len(out[i]) < 3 {
			out[i] = append(out[i], out[i][len(out[i])-1])
		}
	}
	return out, err
}

// Per request, a retriever that returns more candidates for one span than
// maxCandidates is refused, not truncated. The script returns 3 for each
// span: 3 is accepted, 2 is refused.
func TestCandidatesPerSpanAreRefusedNotTruncated(t *testing.T) {
	for _, c := range []struct {
		limit  int
		refuse bool
	}{{3, false}, {2, true}} {
		reg := registryWith(t, func(s *config.SecurityPolicy) { s.MaxCandidates = c.limit })
		p, err := New(reg, overfull{newScripted(reg, example1)}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.Run(context.Background(), "Show customers from India", clock)
		if !c.refuse {
			if err != nil {
				t.Errorf("maxCandidates %d: %v", c.limit, err)
			}
			continue
		}
		if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected || res.Stage != StageRetrieve {
			t.Fatalf("maxCandidates %d: stage %s err %v", c.limit, res.Stage, err)
		}
		if !strings.Contains(err.Error(), "candidates for one phrase, over the limit of 2") {
			t.Errorf("message does not state the reason: %v", err)
		}
	}
}

// lateBy is a clock whose first reading starts the deadline and whose every
// later reading is `by` after it, so each check sees exactly that much
// elapsed time without sleeping.
func lateBy(by time.Duration) func() time.Time {
	base, first := time.Unix(0, 0), true
	return func() time.Time {
		if first {
			first = false
			return base
		}
		return base.Add(by)
	}
}

// The planning deadline covers the deterministic stages and is checked at each
// boundary. Exactly at the limit (100 ms of 100) the request is answered; one
// millisecond over, it is refused at ranking, the first boundary.
func TestPlanningDeadline(t *testing.T) {
	for _, c := range []struct {
		elapsed time.Duration
		refuse  bool
	}{{100 * time.Millisecond, false}, {101 * time.Millisecond, true}} {
		reg := registryWith(t, func(s *config.SecurityPolicy) { s.MaxPlanningMillis = 100 })
		p, err := New(reg, newScripted(reg, example1), Options{})
		if err != nil {
			t.Fatal(err)
		}
		p.now = lateBy(c.elapsed)
		res, err := p.Run(context.Background(), "Show customers from India", clock)
		if !c.refuse {
			if err != nil || res.Stage != StageDone {
				t.Errorf("%v elapsed, limit 100ms: stage %s err %v", c.elapsed, res.Stage, err)
			}
			continue
		}
		if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected || res.Stage != StageRank {
			t.Fatalf("%v elapsed: stage %s err %v", c.elapsed, res.Stage, err)
		}
		if !strings.Contains(err.Error(), "planning took 101 ms by the end of rank, over the limit of 100 ms") {
			t.Errorf("message does not state the reason: %v", err)
		}
		if res.Query.SQL != "" {
			t.Error("a refusal carried SQL")
		}
	}
}

// The deadline is checked after EVERY deterministic stage, not only the
// first: a clock that is on time through ranking and late afterwards is
// refused at binding, planning or compiling, wherever it runs out.
func TestPlanningDeadlineIsCheckedAtEveryStage(t *testing.T) {
	for _, late := range []Stage{StageBind, StagePlan, StageCompile} {
		reg := registryWith(t, func(s *config.SecurityPolicy) { s.MaxPlanningMillis = 100 })
		p, err := New(reg, newScripted(reg, example1), Options{})
		if err != nil {
			t.Fatal(err)
		}
		// Readings: [0] starts the deadline, then one per boundary in order
		// rank, bind, plan, compile. Everything before `late` is on time.
		order := []Stage{StageRank, StageBind, StagePlan, StageCompile}
		reading := 0
		base := time.Unix(0, 0)
		p.now = func() time.Time {
			defer func() { reading++ }()
			if reading == 0 {
				return base
			}
			if order[reading-1] == late {
				return base.Add(time.Second)
			}
			return base
		}
		res, err := p.Run(context.Background(), "Show customers from India", clock)
		if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected || res.Stage != late {
			t.Errorf("late at %s: stage %s err %v", late, res.Stage, err)
		}
	}
}

// Section 16.1 (decision 021): the read-only rule runs in the shared ranking
// path BEFORE ranking's verdict. A write request in which nothing matches is
// refused as a write, not as NO_SEMANTIC_MATCH; one that otherwise compiles
// is refused before binding. Neither carries SQL, and both are SECURITY_REJECTED
// at the rank stage.
//
// The first case was "SELECT * FROM users; DROP TABLE users;" until the Phase
// 8 gate. Its ";" is now refused one stage earlier, at the input boundary
// (decision 022 §3, TestStatementSyntaxNeverReachesAStage), so the case keeps
// what it tests — a write in a sentence that matches nothing — without one.
func TestWritesAreRefusedAsWrites(t *testing.T) {
	for _, c := range []struct {
		sentence string
		hits     map[string][]hit
	}{
		{"SELECT * FROM users DROP TABLE users", nil},
		{"delete customers from India", example1},
	} {
		res, err := run(t, c.sentence, c.hits)
		if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected || res.Stage != StageRank {
			t.Errorf("%q: stage %s err %v", c.sentence, res.Stage, err)
			continue
		}
		if !strings.Contains(err.Error(), "the engine only reads") || res.Query.SQL != "" {
			t.Errorf("%q: %v / %q", c.sentence, err, res.Query.SQL)
		}
	}
}
