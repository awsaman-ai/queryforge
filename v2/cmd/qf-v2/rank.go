package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/numeric"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
	"queryforge.local/v2experiment/internal/retrieval"
)

// This file implements --rank and --rank-probe: the Phase 4 surface.
//
// --rank shows how every candidate reading of a sentence was scored and what
// was decided. It is still NOT the query pipeline: an accepted reading is a set
// of targets, not a query, and binding (5) and compiling (6) stand between. The
// sentence-level refusal (AMBIGUOUS_MATCH / NO_SEMANTIC_MATCH) is returned as
// the exit status so a script can tell a refusal from a reading.
//
// DIAGNOSTIC OUTPUT (decision 013). The report echoes the user's sentence and
// spans, because a ranking decision cannot be judged without the words. It is
// for a developer at a terminal and must never be captured as telemetry or
// logs. The ERROR path is different: VerdictError locates phrases by token
// position and quotes them only under --debug. Config values are redacted on
// both surfaces unless --debug is set (decision 006).

// policyFrom is the ranking policy for this invocation: the config's
// thresholds over the calibrated defaults, unless --min-score / --min-margin
// were given, which override both (and the report says "overridden").
func policyFrom(opt options, reg *config.SchemaRegistry) (ranking.Policy, error) {
	p := pipeline.PolicyFor(reg)
	if opt.thresholdsSet {
		p.MinScore = opt.minScore
		p.MinMargin = opt.minMargin
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// engineRetriever adapts the warm engine to the pipeline's Retriever: one
// batched embedding call, then a top-k search per span.
type engineRetriever struct {
	eng  *engine
	topK int
}

func (r engineRetriever) Retrieve(ctx context.Context, texts []string) ([][]retrieval.Match, error) {
	vectors, err := r.eng.provider.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, qferr.New(qferr.CodeInternal,
			"embedding provider returned %d vectors for %d spans", len(vectors), len(texts))
	}
	out := make([][]retrieval.Match, len(texts))
	for k, v := range vectors {
		m, err := r.eng.index.Search(v, r.topK)
		if err != nil {
			return nil, err
		}
		out[k] = m
	}
	return out, nil
}

// newPipeline is the one orchestration every sentence mode uses (Phase 7):
// --rank, --bind, --plan and --query all rank through the same path.
func newPipeline(eng *engine, opt options, p ranking.Policy) (*pipeline.Pipeline, error) {
	limits := nlp.DefaultOptions
	if opt.maxSpanTokens > 0 {
		limits.MaxSpanTokens = opt.maxSpanTokens
	}
	// CandidateDepth declares --top-k, so a depth over security.maxCandidates
	// is refused at startup rather than on every request (decision 021).
	return pipeline.New(eng.registry, engineRetriever{eng: eng, topK: opt.topK},
		pipeline.Options{Policy: &p, Limits: limits, CandidateDepth: opt.topK})
}

// rankSentence runs extraction → retrieval → ranking for one sentence,
// through the pipeline.
func rankSentence(ctx context.Context, eng *engine, sentence string, opt options, p ranking.Policy) (nlp.Analysis, ranking.Result, error) {
	pl, err := newPipeline(eng, opt, p)
	if err != nil {
		return nlp.Analysis{}, ranking.Result{}, err
	}
	return pl.Rank(ctx, sentence)
}

// warmEngine is newEngine plus a provider even when the index came from disk:
// ranking always has to embed the sentence.
func warmEngine(ctx context.Context, opt options, stderr io.Writer) (*engine, error) {
	eng, err := newEngine(ctx, opt, stderr)
	if err != nil {
		return nil, err
	}
	if eng.provider == nil {
		provider, perr := newProvider(ctx, opt, stderr)
		if perr != nil {
			eng.Close()
			return nil, perr
		}
		eng.provider = provider
	}
	return eng, nil
}

func runRank(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	eng, err := warmEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()
	p, err := policyFrom(opt, eng.registry)
	if err != nil {
		return err
	}

	start := time.Now()
	a, res, err := rankSentence(ctx, eng, opt.rank, opt, p)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	if opt.asJSON {
		if err := emitRankJSON(eng, opt, a, res, elapsed, stdout); err != nil {
			return err
		}
	} else {
		emitRankText(eng, opt, a, res, elapsed, stdout)
	}
	// The same admission --query applies: the read-only rule, then the
	// verdict. Shown after the reading, so the diagnostic still explains it.
	return pipeline.Admit(a, res)
}

// ---------------------------------------------------------------------------
// Text output
// ---------------------------------------------------------------------------

func emitRankText(eng *engine, opt options, a nlp.Analysis, res ranking.Result, elapsed time.Duration, stdout io.Writer) {
	fmt.Fprintf(stdout, "query:    %q\n", opt.rank)
	fmt.Fprintf(stdout, "model:    %s\n", eng.index.Identity())
	fmt.Fprintf(stdout, "policy:   minScore %.2f, minMargin %.2f%s\n",
		res.Policy.MinScore, res.Policy.MinMargin, overriddenNote(opt))
	if opt.debug {
		fmt.Fprintf(stdout, "timing:   embed+retrieve+rank %s\n", elapsed.Round(time.Microsecond))
	}
	fmt.Fprintln(stdout)

	fmt.Fprintln(stdout, "reading (left to right):")
	fmt.Fprintf(stdout, "  %-22s %-10s %-34s %6s %6s %6s %5s\n",
		"span", "decision", "target", "sim", "score", "margin", "conf")

	redacted := 0
	for _, item := range readingItems(a, res) {
		if item.typed != nil {
			fmt.Fprintf(stdout, "  %-22s %-10s %s\n", quoteTrunc(item.typed.Text, 22), "value", typedDescription(*item.typed))
			continue
		}
		s := res.Spans[item.span]
		top, _ := s.TopCandidate()
		fmt.Fprintf(stdout, "  %-22s %-10s %-34s %6.3f %6.3f %6.3f %5.2f\n",
			quoteTrunc(s.Span.Text, 22), s.Decision,
			truncate(top.DisplayTarget(opt.debug), 34),
			top.Similarity, top.Score, s.Margin, s.Confidence)
		if top.MatchedSensitive() && !opt.debug {
			redacted++
		}
		if s.Decision == ranking.Ambiguous {
			for _, j := range s.Competitors {
				c := s.Candidates[j]
				fmt.Fprintf(stdout, "  %-22s %-10s %-34s %6.3f %6.3f\n",
					"", "  vs", truncate(c.DisplayTarget(opt.debug), 34), c.Similarity, c.Score)
				if c.MatchedSensitive() && !opt.debug {
					redacted++
				}
			}
		}
	}

	if len(res.Unexplained) > 0 {
		var words []string
		for _, t := range res.Unexplained {
			words = append(words, fmt.Sprintf("%q", a.Tokens[t].Text))
		}
		fmt.Fprintf(stdout, "\nunexplained: %s\n", strings.Join(words, " "))
	}

	v := string(res.Verdict)
	if v == "" {
		v = "OK — every covered phrase was accepted"
	}
	fmt.Fprintf(stdout, "\nverdict:  %s\n", v)

	if opt.debug {
		emitRankDebug(a, res, stdout)
	} else if redacted > 0 {
		fmt.Fprintf(stdout, "\n(%d target(s) are config values; re-run with --debug to see them)\n", redacted)
	}
}

// emitRankDebug prints every candidate of every covered span with its full
// evidence, then a one-line summary of every other span. --debug only: this is
// the section 12 ranking object, which the brief keeps out of normal output.
func emitRankDebug(a nlp.Analysis, res ranking.Result, stdout io.Writer) {
	inCover := map[int]bool{}
	for _, i := range res.Cover {
		inCover[i] = true
	}

	fmt.Fprintln(stdout, "\n-- candidates for covered spans --")
	for _, i := range res.Cover {
		s := res.Spans[i]
		fmt.Fprintf(stdout, "\n%q tokens[%d:%d]  %s  margin %.3f  confidence %.2f\n",
			s.Span.Text, s.Span.Start, s.Span.End, s.Decision, s.Margin, s.Confidence)
		fmt.Fprintf(stdout, "  %-3s %-34s %6s %5s %5s %5s %5s %5s %5s %5s %6s  %s\n",
			"#", "target", "sim", "lex", "type", "ent", "op", "rel", "ctx", "span", "score", "matched")
		for k, c := range s.Candidates {
			e := c.Evidence
			mark := ""
			switch {
			case k == s.Top:
				mark = "*"
			case k == s.RunnerUp:
				mark = "2"
			case c.Vetoed:
				mark = "x"
			}
			fmt.Fprintf(stdout, "  %-3s %-34s %6.3f %5.2f %5.2f %5.2f %5.2f %5.2f %5.2f %5.2f %6.3f  %s\n",
				mark, truncate(c.DisplayTarget(true), 34), c.Similarity,
				e.Lexical, e.Type, e.Entity, e.Operator, e.Relationship, e.Context, e.Span,
				c.Score, c.MatchedText(true))
			for _, r := range c.Reasons {
				fmt.Fprintf(stdout, "        - %s\n", r)
			}
		}
	}

	fmt.Fprintln(stdout, "\n-- every other span --")
	for i, s := range res.Spans {
		if inCover[i] {
			continue
		}
		top, ok := s.TopCandidate()
		target := "—"
		if ok {
			target = top.DisplayTarget(true)
		}
		fmt.Fprintf(stdout, "  %-26s %-10s %-18s %-34s score %.3f margin %.3f\n",
			quoteTrunc(s.Span.Text, 26), s.Decision, s.Code, truncate(target, 34), scoreOf(s), s.Margin)
	}
}

// readingItem is one entry of the left-to-right reading: a covered text span
// or a typed value.
type readingItem struct {
	span  int
	typed *nlp.Span
	start int
}

func readingItems(a nlp.Analysis, res ranking.Result) []readingItem {
	var items []readingItem
	for _, i := range res.Cover {
		items = append(items, readingItem{span: i, start: res.Spans[i].Span.Start})
	}
	for _, s := range a.Spans {
		if s.Kind == nlp.SpanNumber || s.Kind == nlp.SpanDate {
			ss := s
			items = append(items, readingItem{span: -1, typed: &ss, start: s.Start})
		}
	}
	// Insertion sort by position; a handful of items.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].start < items[j-1].start; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	return items
}

func typedDescription(s nlp.Span) string {
	switch {
	case s.Number != nil && s.Number.Malformed:
		return "number (malformed, not read)"
	case s.Number != nil:
		return "number " + s.Number.Value.String()
	case s.Date != nil:
		return "date " + describeDate(*s.Date)
	}
	return string(s.Kind)
}

func describeDate(d nlp.DateExpr) string {
	return fmt.Sprintf("%s %s, count %d, offset %d", d.Anchor, d.Unit, d.Count, d.Offset)
}

func scoreOf(s ranking.SpanRanking) float64 {
	if c, ok := s.TopCandidate(); ok {
		return c.Score
	}
	return 0
}

func overriddenNote(opt options) string {
	if opt.thresholdsSet {
		return "   (OVERRIDDEN on the command line — not the calibrated policy)"
	}
	return ""
}

func quoteTrunc(s string, width int) string {
	return truncate(fmt.Sprintf("%q", s), width)
}

// ---------------------------------------------------------------------------
// JSON output
// ---------------------------------------------------------------------------

type jsonCandidate struct {
	Target     string            `json:"target"`
	Kind       string            `json:"kind"`
	Similarity float64           `json:"similarity"`
	Score      float64           `json:"score"`
	Evidence   *ranking.Evidence `json:"evidence,omitempty"`
	Matched    string            `json:"matched,omitempty"`
	Redacted   bool              `json:"redacted,omitempty"`
	Vetoed     bool              `json:"vetoed,omitempty"`
	Reasons    []string          `json:"reasons,omitempty"`
}

type jsonSpan struct {
	Span        string          `json:"span"`
	Tokens      [2]int          `json:"tokens"`
	Decision    string          `json:"decision"`
	Code        string          `json:"code,omitempty"`
	Target      *jsonCandidate  `json:"target,omitempty"`
	Margin      float64         `json:"margin"`
	Confidence  float64         `json:"confidence"`
	Competitors []jsonCandidate `json:"competitors,omitempty"`
	Candidates  []jsonCandidate `json:"candidates,omitempty"`
}

func toJSONCandidate(c ranking.Candidate, reveal, full bool) jsonCandidate {
	out := jsonCandidate{
		Target:     c.DisplayTarget(reveal),
		Kind:       string(c.Entry.Kind),
		Similarity: c.Similarity,
		Score:      c.Score,
		Vetoed:     c.Vetoed,
	}
	if full {
		e := c.Evidence
		out.Evidence = &e
		out.Matched = c.MatchedText(reveal)
		out.Redacted = c.MatchedSensitive() && !reveal
		out.Reasons = c.Reasons
	}
	return out
}

func toJSONSpan(s ranking.SpanRanking, reveal, full bool) jsonSpan {
	out := jsonSpan{
		Span:       s.Span.Text,
		Tokens:     [2]int{s.Span.Start, s.Span.End},
		Decision:   string(s.Decision),
		Code:       string(s.Code),
		Margin:     s.Margin,
		Confidence: s.Confidence,
	}
	if c, ok := s.TopCandidate(); ok {
		jc := toJSONCandidate(c, reveal, full)
		out.Target = &jc
	}
	for _, j := range s.Competitors {
		out.Competitors = append(out.Competitors, toJSONCandidate(s.Candidates[j], reveal, full))
	}
	if full {
		for _, c := range s.Candidates {
			out.Candidates = append(out.Candidates, toJSONCandidate(c, reveal, true))
		}
	}
	return out
}

func emitRankJSON(eng *engine, opt options, a nlp.Analysis, res ranking.Result, elapsed time.Duration, stdout io.Writer) error {
	type jsonValue struct {
		Span   string           `json:"span"`
		Tokens [2]int           `json:"tokens"`
		Kind   string           `json:"kind"`
		Number *numeric.Decimal `json:"number,omitempty"`
		Date   string           `json:"date,omitempty"`
	}
	out := struct {
		Query       string         `json:"query"`
		Model       any            `json:"model"`
		Policy      ranking.Policy `json:"policy"`
		Overridden  bool           `json:"thresholdsOverridden,omitempty"`
		Verdict     string         `json:"verdict"`
		Reading     []jsonSpan     `json:"reading"`
		Values      []jsonValue    `json:"values,omitempty"`
		Unexplained []string       `json:"unexplained,omitempty"`
		RankMS      float64        `json:"rankMs"`
		// AllSpans is the full ranking object — --debug only (section 12).
		AllSpans []jsonSpan `json:"allSpans,omitempty"`
	}{
		Query:      opt.rank,
		Model:      eng.index.Identity(),
		Policy:     res.Policy,
		Overridden: opt.thresholdsSet,
		Verdict:    string(res.Verdict),
		RankMS:     float64(elapsed.Microseconds()) / 1000,
	}
	if out.Verdict == "" {
		out.Verdict = "OK"
	}
	for _, i := range res.Cover {
		out.Reading = append(out.Reading, toJSONSpan(res.Spans[i], opt.debug, opt.debug))
	}
	for _, s := range a.Spans {
		switch {
		case s.Kind == nlp.SpanNumber && s.Number != nil:
			jv := jsonValue{Span: s.Text, Tokens: [2]int{s.Start, s.End}, Kind: "number"}
			if !s.Number.Malformed {
				v := s.Number.Value
				jv.Number = &v
			}
			out.Values = append(out.Values, jv)
		case s.Kind == nlp.SpanDate && s.Date != nil:
			out.Values = append(out.Values, jsonValue{Span: s.Text, Tokens: [2]int{s.Start, s.End}, Kind: "date", Date: describeDate(*s.Date)})
		}
	}
	for _, t := range res.Unexplained {
		out.Unexplained = append(out.Unexplained, a.Tokens[t].Text)
	}
	if opt.debug {
		for _, s := range res.Spans {
			out.AllSpans = append(out.AllSpans, toJSONSpan(s, true, true))
		}
	}
	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "rank output could not be encoded")
	}
	fmt.Fprintln(stdout, string(enc))
	return nil
}

// ---------------------------------------------------------------------------
// --rank-probe: ranking versus raw top-1 on an ambiguity set
// ---------------------------------------------------------------------------

// expectAmbiguous is the expected column for rows where the only correct
// behaviour is to refuse to pick.
const expectAmbiguous = "AMBIGUOUS"

type rankCase struct {
	Sentence string
	// SpanText is the phrase under test, as it appears in the sentence.
	SpanText string
	// Expected is a corpus entry id, AMBIGUOUS, or "" for exploratory rows.
	Expected string
	Note     string
	Line     int
}

// outcome classifies one method's answer on one row.
type outcome string

const (
	outcomeCorrect outcome = "correct"
	// outcomeRefused is a safe miss: the method declined to pick, when a pick
	// was expected. Costs a reprompt, not a wrong query.
	outcomeRefused outcome = "refused"
	// outcomeWrong is the failure that matters: a confident, wrong pick. On an
	// AMBIGUOUS row, any pick at all is this — it is a silent guess.
	outcomeWrong outcome = "WRONG"
)

func loadRankCases(path string) ([]rankCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, qferr.Wrap(qferr.CodeInvalidInput, err, "rank probe file could not be read: %s", path)
	}
	defer f.Close()

	var cases []rankCase
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 2 {
			return nil, qferr.New(qferr.CodeInvalidInput,
				"rank probe line %d: want sentence<TAB>span<TAB>expected<TAB>note", line)
		}
		c := rankCase{Sentence: strings.TrimSpace(parts[0]), SpanText: strings.TrimSpace(parts[1]), Line: line}
		if len(parts) > 2 {
			c.Expected = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 {
			c.Note = strings.TrimSpace(parts[3])
		}
		if c.Sentence == "" || c.SpanText == "" {
			return nil, qferr.New(qferr.CodeInvalidInput, "rank probe line %d has an empty sentence or span", line)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, qferr.Wrap(qferr.CodeInvalidInput, err, "rank probe file could not be read")
	}
	if len(cases) == 0 {
		return nil, qferr.New(qferr.CodeInvalidInput, "rank probe file contains no rows")
	}
	return cases, nil
}

type rankRow struct {
	Case   rankCase
	Scored bool
	// Raw is retrieval's top-1 for the phrase alone; Threshold is the same pick
	// gated by MinScore only; Ranked is the Phase 4 decision.
	Raw, Threshold, Ranked outcome

	rawEntryID     string // internal id; rendered through corpus.DisplayID
	RawScore       float64
	rankedEntryID  string // internal id; rendered through corpus.DisplayID
	RankedDecision string
	RankedMargin   float64
}

func runRankProbe(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	cases, err := loadRankCases(opt.rankProbe)
	if err != nil {
		return err
	}
	eng, err := warmEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()
	p, err := policyFrom(opt, eng.registry)
	if err != nil {
		return err
	}

	var rows []rankRow
	for _, c := range cases {
		a, res, err := rankSentence(ctx, eng, c.Sentence, opt, p)
		if err != nil {
			return err
		}
		row, err := evaluateRankCase(c, a, res, p)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	emitRankProbe(eng, opt, p, rows, stdout)
	return nil
}

// evaluateRankCase scores the three methods on one row.
func evaluateRankCase(c rankCase, a nlp.Analysis, res ranking.Result, p ranking.Policy) (rankRow, error) {
	row := rankRow{Case: c, Scored: c.Expected != ""}

	// The span under test. Every text n-gram is a span, so a row naming a
	// phrase the sentence does not contain is a broken row, not a miss.
	want := nlp.FoldCase(nlp.Normalize(c.SpanText))
	soi := -1
	for i, s := range res.Spans {
		if s.Span.Norm == want {
			soi = i
			break
		}
	}
	if soi < 0 {
		return row, qferr.New(qferr.CodeInvalidInput,
			"rank probe line %d: the sentence has no span matching the phrase under test", c.Line)
	}
	at := res.Spans[soi].Span

	// Baselines: retrieval's own top-1 for that span. Generous on purpose —
	// the baseline is handed the right span, where ranking has to find it.
	raw := res.Spans[soi]
	var rawTop *ranking.Candidate
	for k := range raw.Candidates {
		// Candidates are re-sorted by ranking score; the raw top-1 is the
		// highest SIMILARITY, with retrieval's own id tie-break.
		cand := raw.Candidates[k]
		if rawTop == nil || cand.Similarity > rawTop.Similarity ||
			(cand.Similarity == rawTop.Similarity && cand.Entry.ID < rawTop.Entry.ID) {
			rawTop = &raw.Candidates[k]
		}
	}
	if rawTop != nil {
		row.rawEntryID = rawTop.Entry.ID
		row.RawScore = rawTop.Similarity
	}

	// Ranked: the covered span that explains the phrase under test — the one
	// overlapping it most, since the cover may have chosen a longer or shorter
	// reading of the same words.
	best, bestOverlap := -1, 0
	for _, i := range res.Cover {
		s := res.Spans[i].Span
		ov := minInt(s.End, at.End) - maxInt(s.Start, at.Start)
		if ov > bestOverlap {
			best, bestOverlap = i, ov
		}
	}
	rankedDecision := ranking.Reject
	rankedID := ""
	if best >= 0 {
		s := res.Spans[best]
		rankedDecision = s.Decision
		row.RankedMargin = s.Margin
		if top, ok := s.TopCandidate(); ok && s.Decision == ranking.Accept {
			rankedID = top.Entry.ID
		}
	}
	row.RankedDecision = string(rankedDecision)
	row.rankedEntryID = rankedID

	if !row.Scored {
		return row, nil
	}

	// A pick is correct only if it is the expected id; on an AMBIGUOUS row
	// every pick is a silent guess.
	judgePick := func(id string) outcome {
		if c.Expected != expectAmbiguous && id == c.Expected {
			return outcomeCorrect
		}
		return outcomeWrong
	}

	if rawTop == nil {
		row.Raw = outcomeRefused
	} else {
		row.Raw = judgePick(rawTop.Entry.ID)
	}
	if rawTop == nil || rawTop.Similarity+1e-9 < p.MinScore {
		row.Threshold = outcomeRefused
	} else {
		row.Threshold = judgePick(rawTop.Entry.ID)
	}

	switch rankedDecision {
	case ranking.Accept:
		row.Ranked = judgePick(rankedID)
	case ranking.Ambiguous:
		if c.Expected == expectAmbiguous {
			row.Ranked = outcomeCorrect
		} else {
			row.Ranked = outcomeRefused
		}
	default:
		// Rejected: nothing picked. Safe on either kind of row, but only an
		// AMBIGUOUS decision is counted correct there — "I found nothing" is
		// not the same claim as "I found two".
		row.Ranked = outcomeRefused
	}
	return row, nil
}

func emitRankProbe(eng *engine, opt options, p ranking.Policy, rows []rankRow, stdout io.Writer) {
	id := eng.index.Identity()
	fmt.Fprintf(stdout, "## Phase 4 ranking probe — %s\n\n", opt.rankProbe)
	fmt.Fprintf(stdout, "- config: `%s`\n- model: `%s`\n- policy: minScore %.2f, minMargin %.2f%s\n\n",
		opt.configPath, id, p.MinScore, p.MinMargin, overriddenNote(opt))
	if !id.Semantic {
		fmt.Fprintln(stdout, "> **NOT SEMANTIC** — hashing embedder; a mechanism check, not a finding.")
		fmt.Fprintln(stdout)
	}

	fmt.Fprintln(stdout, "| sentence | span | expected | raw top-1 | sim | raw | threshold-only | ranked | decision | margin | ranked |")
	fmt.Fprintln(stdout, "|---|---|---|---|---|---|---|---|---|---|---|")

	type tally struct{ correct, refused, wrong int }
	var raw, thr, rk tally
	count := func(t *tally, o outcome) {
		switch o {
		case outcomeCorrect:
			t.correct++
		case outcomeRefused:
			t.refused++
		case outcomeWrong:
			t.wrong++
		}
	}
	scored := 0
	for _, r := range rows {
		exp := r.Case.Expected
		if exp == "" {
			exp = "—"
		}
		rankedTarget := "—"
		if r.rankedEntryID != "" {
			rankedTarget = shortID(corpus.DisplayID(r.rankedEntryID, opt.debug))
		}
		fmt.Fprintf(stdout, "| %s | %s | %s | %s | %.3f | %s | %s | %s | %s | %.3f | %s |\n",
			r.Case.Sentence, r.Case.SpanText, shortID(corpus.DisplayID(exp, opt.debug)),
			shortID(corpus.DisplayID(r.rawEntryID, opt.debug)), r.RawScore,
			dash(r.Raw), dash(r.Threshold), rankedTarget, r.RankedDecision, r.RankedMargin, dash(r.Ranked))
		if r.Scored {
			scored++
			count(&raw, r.Raw)
			count(&thr, r.Threshold)
			count(&rk, r.Ranked)
		}
	}

	if scored == 0 {
		return
	}
	pct := func(n int) float64 { return 100 * float64(n) / float64(scored) }
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "| method | correct | refused (safe) | silently wrong |")
	fmt.Fprintln(stdout, "|---|---|---|---|")
	for _, m := range []struct {
		name string
		t    tally
	}{{"raw vector top-1", raw}, {"threshold only", thr}, {"Phase 4 ranking", rk}} {
		fmt.Fprintf(stdout, "| %s | %d/%d (%.0f%%) | %d | %d |\n",
			m.name, m.t.correct, scored, pct(m.t.correct), m.t.refused, m.t.wrong)
	}
	// One machine-readable line for the gate script.
	fmt.Fprintf(stdout, "\nRANKPROBE raw=%d threshold=%d ranked=%d scored=%d raw_wrong=%d threshold_wrong=%d ranked_wrong=%d\n",
		raw.correct, thr.correct, rk.correct, scored, raw.wrong, thr.wrong, rk.wrong)
}

func dash(o outcome) string {
	if o == "" {
		return "—"
	}
	return string(o)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
