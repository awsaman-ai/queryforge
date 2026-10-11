// Package pipeline is the Phase 7 orchestration: one natural-language request
// through every stage, in the one order the experiment allows.
//
//	sentence ─▶ extract (nlp) ─▶ retrieve (embedding + vector index) ─▶ rank
//	         ─▶ bind (schema graph) ─▶ plan (SemanticPlan) ─▶ compile ─▶ SQL + params
//
// Section 15 of the brief: "Connect Phases 1–6 into the first complete
// experiment." Every stage is the one its phase built. This package adds no
// semantic rule; it sequences, times, and stops at the first refusal.
//
// NO BYPASS. The compiler's only input is a plan.Plan, and the only way to a
// plan from a sentence is plan.FromBinding. Nothing here builds SQL, and
// nothing here generates anything: the embedding model's role ends at
// retrieval, and every later stage is deterministic code over the config.
//
// A Pipeline is immutable after New and safe to share across goroutines, if
// its Retriever is (section 22, rule 9).
package pipeline

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"queryforge.local/v2experiment/internal/binding"
	"queryforge.local/v2experiment/internal/compiler"
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/inputguard"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
	"queryforge.local/v2experiment/internal/readonly"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Retriever finds corpus candidates for span texts: embedding plus vector
// search, in one call so a provider can batch. It is the pipeline's only
// dependency on the model, and tests replace it with a script.
type Retriever interface {
	Retrieve(ctx context.Context, texts []string) ([][]retrieval.Match, error)
}

// Stage names a pipeline step.
type Stage string

const (
	StageInput    Stage = "input"
	StageExtract  Stage = "extract"
	StageRetrieve Stage = "retrieve"
	StageRank     Stage = "rank"
	StageBind     Stage = "bind"
	StagePlan     Stage = "plan"
	StageCompile  Stage = "compile"
	StageDone     Stage = "done"
)

// Options configure a pipeline.
type Options struct {
	// Policy, when set, overrides the ranking policy: the command line's
	// explicit thresholds. Otherwise the config's `ranking` block applies
	// over the calibrated defaults.
	Policy *ranking.Policy
	// Limits bound phrase extraction. Zero value → nlp.DefaultOptions. The
	// span limit itself is NOT taken from here: New replaces MaxSpans with the
	// config's security.maxPhrases, so the config is the one place that bounds
	// a request (decision 021).
	Limits nlp.Options
	// CandidateDepth is how many candidates the Retriever returns per span
	// (the CLI's --top-k). When set, New refuses a depth over the config's
	// security.maxCandidates, so a misconfigured engine fails at startup
	// rather than on every request. Zero means "not declared"; the per-request
	// check in rank still applies either way.
	CandidateDepth int
}

// Pipeline runs requests against one config and one warm retriever.
type Pipeline struct {
	reg       *config.SchemaRegistry
	graph     *graph.Graph
	retriever Retriever
	policy    ranking.Policy
	limits    nlp.Options
	// now is the clock the planning deadline reads. Always time.Now outside
	// tests; a test swaps it to make the deadline fire deterministically.
	now func() time.Time
}

// New builds a pipeline. The policy is resolved once here — defaults, then the
// config's thresholds, then an explicit override — so every request of the
// pipeline's life ranks under the same policy.
func New(reg *config.SchemaRegistry, r Retriever, opt Options) (*Pipeline, error) {
	if reg == nil || r == nil {
		return nil, qferr.New(qferr.CodeInternal, "a pipeline needs a registry and a retriever")
	}
	pol := PolicyFor(reg)
	if opt.Policy != nil {
		pol = *opt.Policy
	}
	if err := pol.Validate(); err != nil {
		return nil, err
	}
	limits := opt.Limits
	if limits == (nlp.Options{}) {
		limits = nlp.DefaultOptions
	}
	sec := reg.Security()
	// Section 16.7's phrase limit is the config's, whatever the extraction
	// defaults say.
	limits.MaxSpans = sec.MaxPhrases
	if opt.CandidateDepth < 0 {
		return nil, qferr.New(qferr.CodeConfigError, "the retrieval depth must not be negative (got %d)", opt.CandidateDepth)
	}
	if max := sec.MaxCandidates; max > 0 && opt.CandidateDepth > max {
		return nil, qferr.New(qferr.CodeConfigError,
			"retrieval depth %d is over security.maxCandidates (%d): lower the depth or raise the limit", opt.CandidateDepth, max)
	}
	return &Pipeline{reg: reg, graph: graph.New(reg), retriever: r, policy: pol, limits: limits, now: time.Now}, nil
}

// PolicyFor is the calibrated default policy with the config's thresholds
// applied.
func PolicyFor(reg *config.SchemaRegistry) ranking.Policy {
	pol := ranking.DefaultPolicy()
	if t := reg.Config().Ranking; t.MinScore != nil || t.MinMargin != nil {
		if t.MinScore != nil {
			pol.MinScore = *t.MinScore
		}
		if t.MinMargin != nil {
			pol.MinMargin = *t.MinMargin
		}
	}
	return pol
}

// Policy is the ranking policy this pipeline applies.
func (p *Pipeline) Policy() ranking.Policy { return p.policy }

// Registry is the pipeline's schema registry.
func (p *Pipeline) Registry() *config.SchemaRegistry { return p.reg }

// Graph is the pipeline's schema graph.
func (p *Pipeline) Graph() *graph.Graph { return p.graph }

// Result is everything one request produced, up to the stage it reached. On a
// refusal, the fields of the stages before it are filled — so a refusal can be
// explained — and Stage names the stage that refused.
type Result struct {
	Stage    Stage
	Analysis nlp.Analysis
	Ranking  ranking.Result
	Binding  binding.Binding
	Plan     plan.Plan
	Query    compiler.Query
	Stats    Stats
}

// Run takes one sentence to a final query, or to the first refusal.
func (p *Pipeline) Run(ctx context.Context, sentence string, clock plan.Clock) (Result, error) {
	start := time.Now()
	var res Result
	finish := func(stage Stage, err error) (Result, error) {
		res.Stage = stage
		res.Stats.fill(res, stage, err, time.Since(start))
		return res, err
	}

	if err := p.checkInput(sentence); err != nil {
		return finish(StageInput, err)
	}
	res.Stats.InputLength = utf8.RuneCountInString(sentence)

	var err error
	var stage Stage
	var planning deadline
	res.Analysis, res.Ranking, stage, planning, err = p.rank(ctx, sentence, &res.Stats)
	if err == nil {
		err = Admit(res.Analysis, res.Ranking)
		stage = StageRank
	}
	if err == nil {
		err = planning.check(StageRank)
	}
	if err != nil {
		return finish(stage, err)
	}

	t := time.Now()
	res.Binding, err = binding.Bind(binding.Input{Analysis: res.Analysis, Ranking: res.Ranking, Registry: p.reg, Graph: p.graph})
	res.Stats.BindMs = ms(time.Since(t))
	if err == nil {
		err = planning.check(StageBind)
	}
	if err != nil {
		return finish(StageBind, err)
	}

	t = time.Now()
	res.Plan, err = plan.FromBinding(res.Binding, p.reg, clock)
	res.Stats.PlanMs = ms(time.Since(t))
	if err == nil {
		err = planning.check(StagePlan)
	}
	if err != nil {
		return finish(StagePlan, err)
	}

	t = time.Now()
	res.Query, err = compiler.Compile(res.Plan, p.reg, p.graph)
	res.Stats.CompileMs = ms(time.Since(t))
	if err == nil {
		err = planning.check(StageCompile)
	}
	if err != nil {
		return finish(StageCompile, err)
	}
	return finish(StageDone, nil)
}

// deadline is section 16.7's planning-time limit: the wall time of the
// deterministic stages — ranking, binding, planning, compiling — measured from
// the end of retrieval. Retrieval is excluded on purpose: its time is the
// embedding worker's, which Phase 9 measures separately, and a slow worker is
// not a sign that the reasoning is running away.
//
// It is checked at each stage boundary. A stage is not interrupted mid-way;
// each is bounded by the other limits (spans, candidates, joins, filters), so
// the overshoot is at most one stage. A request over the limit is refused,
// never answered late: an answer produced after the limit is still refused,
// because the limit exists to bound the work the engine agrees to do.
//
// This is the one refusal that depends on the machine rather than on the
// sentence and the config. At the default (2000 ms) against measured stage
// times under 10 ms, it is a safety net, not a verdict an ordinary request
// can meet.
type deadline struct {
	start time.Time
	max   time.Duration
	now   func() time.Time
}

func (d deadline) check(after Stage) error {
	if d.now == nil || d.max <= 0 {
		return nil
	}
	if took := d.now().Sub(d.start); took > d.max {
		return qferr.New(qferr.CodeSecurityRejected,
			"planning took %d ms by the end of %s, over the limit of %d ms", took.Milliseconds(), after, d.max.Milliseconds())
	}
	return nil
}

// Rank runs the first three stages only — extraction, retrieval, ranking — for
// the diagnostic modes (--rank, --bind, --plan). They share this path with
// Run, so a diagnostic never ranks differently from a real request. Unlike
// Run, it returns ranking's result even when the sentence verdict is a
// refusal: showing that verdict is what --rank is for.
func (p *Pipeline) Rank(ctx context.Context, sentence string) (nlp.Analysis, ranking.Result, error) {
	if err := p.checkInput(sentence); err != nil {
		return nlp.Analysis{}, ranking.Result{}, err
	}
	var st Stats
	a, r, _, _, err := p.rank(ctx, sentence, &st)
	return a, r, err
}

// Admit is every refusal a reading gets before binding, in order: section
// 16.1's read-only rule, then ranking's own verdict. The read-only rule goes
// first so a write request is refused AS a write — "SELECT * FROM users; DROP
// TABLE users" matches nothing, and saying so would hide the real reason
// (decision 021).
//
// Run applies it; the diagnostic modes apply it too, after showing the
// reading, so no diagnostic binds or plans a sentence --query refuses.
func Admit(a nlp.Analysis, r ranking.Result) error {
	if err := readonly.Check(a, r); err != nil {
		return err
	}
	return r.Err()
}

// checkInput applies section 16.7's input-length limit and the input
// boundary: refusals, never a truncation or a stripped delimiter — a trimmed
// request is a different request.
func (p *Pipeline) checkInput(sentence string) error {
	if strings.TrimSpace(sentence) == "" {
		return qferr.New(qferr.CodeInvalidInput, "the request is empty")
	}
	if max := p.reg.Security().MaxInputLength; max > 0 && utf8.RuneCountInString(sentence) > max {
		return qferr.New(qferr.CodeInvalidInput, "the request is %d characters, over the limit of %d",
			utf8.RuneCountInString(sentence), max)
	}
	// The input boundary (decision 022 §3): one request is one query, so SQL
	// statement and comment syntax is refused here, before extraction reads a
	// word of it. Run and Rank both pass through this function, so no mode —
	// --query, --rank, --bind, --plan, the e2e runner, or api.ToSQL — reads a
	// request this refuses.
	return inputguard.Check(sentence)
}

// rank runs extraction, retrieval and ranking. It also returns the planning
// deadline, started when retrieval ends, which Run checks after every later
// stage.
func (p *Pipeline) rank(ctx context.Context, sentence string, st *Stats) (nlp.Analysis, ranking.Result, Stage, deadline, error) {
	var dl deadline
	t := time.Now()
	a := nlp.Analyze(sentence, p.limits)
	st.ExtractMs = ms(time.Since(t))
	st.PhraseCount = len(a.Spans)
	// security.maxPhrases, checked before retrieval: the spans are what the
	// request would embed and search, so this is the check that bounds that
	// work. A refusal, never a truncation.
	if max := p.limits.MaxSpans; max > 0 && len(a.Spans) > max {
		return a, ranking.Result{}, StageExtract, dl, qferr.New(qferr.CodeInvalidInput,
			"input produced %d candidate spans, over the limit of %d", len(a.Spans), max)
	}

	// One batched call for every text span: per-span round trips to the
	// embedding worker would dominate latency for no reason.
	var texts []string
	var owners []int
	for i, s := range a.Spans {
		if s.Kind == nlp.SpanText {
			texts = append(texts, s.Norm)
			owners = append(owners, i)
		}
	}
	retrieved := make([][]retrieval.Match, len(a.Spans))
	t = time.Now()
	if len(texts) > 0 {
		found, err := p.retriever.Retrieve(ctx, texts)
		if err != nil {
			return a, ranking.Result{}, StageRetrieve, dl, err
		}
		if len(found) != len(texts) {
			return a, ranking.Result{}, StageRetrieve, dl, qferr.New(qferr.CodeInternal,
				"retrieval returned %d results for %d spans", len(found), len(texts))
		}
		max := p.reg.Security().MaxCandidates
		for k, m := range found {
			// security.maxCandidates bounds ONE span's candidates (decision
			// 021). New already refuses a declared depth over it; this catches
			// a retriever that returns more than it declared, or declared
			// nothing. Refused, never truncated: dropping the tail would
			// change which runner-up the margin is measured against.
			if max > 0 && len(m) > max {
				return a, ranking.Result{}, StageRetrieve, dl, qferr.New(qferr.CodeSecurityRejected,
					"retrieval returned %d candidates for one phrase, over the limit of %d", len(m), max)
			}
			retrieved[owners[k]] = m
			st.CandidateCount += len(m)
		}
	}
	st.RetrieveMs = ms(time.Since(t))

	dl = deadline{start: p.now(), max: time.Duration(p.reg.Security().MaxPlanningMillis) * time.Millisecond, now: p.now}
	t = time.Now()
	res, err := ranking.Rank(ranking.Input{Analysis: a, Retrieved: retrieved, Registry: p.reg}, p.policy)
	st.RankMs = ms(time.Since(t))
	return a, res, StageRank, dl, err
}

// Stats is the request's telemetry (section 24), built ONLY from structured,
// redacted forms (decision 013): lengths, counts, timings, schema names and
// codes. It holds no user text and no values, so it may be logged. A test
// marshals it after a request full of sensitive words and checks none appear.
type Stats struct {
	Stage     Stage      `json:"phase"`
	ErrorCode qferr.Code `json:"error_code,omitempty"`

	InputLength    int `json:"input_length"`
	PhraseCount    int `json:"phrase_count"`
	CandidateCount int `json:"candidate_count"`

	// RetrieveMs covers embedding and vector search together: the Retriever
	// does both in one call. Phase 9 splits them if it needs to.
	ExtractMs  float64 `json:"extraction_latency_ms"`
	RetrieveMs float64 `json:"retrieval_latency_ms"`
	RankMs     float64 `json:"ranking_latency_ms"`
	BindMs     float64 `json:"binding_latency_ms"`
	PlanMs     float64 `json:"planning_latency_ms"`
	CompileMs  float64 `json:"compilation_latency_ms"`
	TotalMs    float64 `json:"total_latency_ms"`

	// Confidence is the lowest decision confidence among the accepted spans
	// the reading used: the request is only as sure as its least sure part.
	Confidence        float64  `json:"confidence"`
	SelectedEntity    string   `json:"selected_entity,omitempty"`
	SelectedFields    []string `json:"selected_fields,omitempty"`
	SelectedOperators []string `json:"selected_operators,omitempty"`
	JoinCount         int      `json:"join_count"`
	ParamCount        int      `json:"param_count"`
	QueryLength       int      `json:"query_length"`
}

func (s *Stats) fill(r Result, stage Stage, err error, total time.Duration) {
	s.Stage = stage
	s.TotalMs = ms(total)
	if err != nil {
		s.ErrorCode = qferr.CodeOf(err)
	}
	first := true
	for _, i := range r.Ranking.Cover {
		sr := r.Ranking.Spans[i]
		if sr.Decision != ranking.Accept {
			continue
		}
		if first || sr.Confidence < s.Confidence {
			s.Confidence, first = sr.Confidence, false
		}
	}
	if stage == StagePlan || stage == StageCompile || stage == StageDone {
		s.SelectedEntity = r.Binding.Root
		for _, j := range r.Binding.Joins {
			s.JoinCount += j.Len()
		}
	}
	if stage == StageCompile || stage == StageDone {
		fields, ops := map[string]bool{}, map[string]bool{}
		for _, f := range r.Plan.Select {
			fields[f.Field] = true
		}
		for _, a := range r.Plan.Aggregates {
			if a.Field != "" {
				fields[a.Field] = true
			}
		}
		for _, c := range r.Plan.Where {
			if c.Field != "" {
				fields[c.Field] = true
			}
			if c.Op != "" {
				ops[string(c.Op)] = true
			}
		}
		for _, o := range r.Plan.OrderBy {
			if o.Field != "" {
				fields[o.Field] = true
			}
		}
		s.SelectedFields, s.SelectedOperators = sorted(fields), sorted(ops)
	}
	if stage == StageDone {
		s.ParamCount = len(r.Query.Params)
		s.QueryLength = len(r.Query.SQL)
	}
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
