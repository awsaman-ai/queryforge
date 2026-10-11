package nlp

// SpanKind says what a span is, lexically.
type SpanKind string

const (
	// SpanText is an ordinary candidate phrase. What it refers to is decided by
	// retrieval and ranking, not here.
	SpanText SpanKind = "text"
	// SpanNumber is a numeric literal.
	SpanNumber SpanKind = "number"
	// SpanDate is a recognised date expression.
	SpanDate SpanKind = "date"
)

// Connective is a logical joiner between conditions.
type Connective struct {
	// Op is "and", "or" or "not".
	Op         string
	Text       string
	Start, End int
}

// Span is a contiguous candidate piece of the sentence.
type Span struct {
	Kind SpanKind
	// Text is the surface form as written.
	Text string
	// Norm is the case-folded form, which is what gets embedded.
	Norm string
	// Start, End are token indices, half-open.
	Start, End int
	// ByteStart, ByteEnd locate the span in the normalised input, so a rejected
	// candidate can be pointed at in the user's own words.
	ByteStart, ByteEnd int
	// Number and Date carry the parsed payload for their kinds.
	Number *Number
	Date   *DateExpr
	// AllStop marks a span made only of function words. Kept out of the
	// candidate set, but recorded when explaining why.
	AllStop bool
}

// Analysis is everything Phase 3 produces for one sentence.
type Analysis struct {
	Input      string
	Normalized string
	Tokens     []Token
	// Spans are the candidate phrases, ordered longest-first then
	// left-to-right, so a caller that takes the first match prefers the more
	// specific reading.
	Spans       []Span
	Numbers     []Number
	Dates       []DateExpr
	Connectives []Connective
}

// Options tunes span generation.
type Options struct {
	// MaxSpanTokens caps candidate phrase length. Four covers the configured
	// phrases this experiment has ("greater than or equal to" is the longest)
	// with room to spare; longer spans embed to mush and multiply the candidate
	// set for nothing.
	MaxSpanTokens int
	// MaxSpans bounds the candidate set. Section 16.7 requires a limit on the
	// number of phrases; exceeding it is a refusal, never a silent truncation,
	// so this is enforced by the caller rather than by dropping spans here.
	MaxSpans int
}

// DefaultOptions are the Phase 3 starting points. Calibrated in Phase 9 against
// measurements rather than defended as optimal now.
var DefaultOptions = Options{MaxSpanTokens: 4, MaxSpans: 256}

var connectiveWords = map[string]string{
	"and": "and", "or": "or", "nor": "or",
	"not": "not", "except": "not", "excluding": "not", "without": "not",
}

// Analyze runs the whole Phase 3 pipeline over one sentence.
//
// The order matters: numbers are found before dates, because "last 6 months"
// needs the 6 already parsed, and dates before spans, so a date's tokens can be
// marked and not also emitted as meaningless n-grams.
func Analyze(input string, opts Options) Analysis {
	if opts.MaxSpanTokens <= 0 {
		opts.MaxSpanTokens = DefaultOptions.MaxSpanTokens
	}

	normalized := Normalize(input)
	tokens := Tokenize(normalized)

	numbers := ExtractNumbers(tokens)
	dates := ExtractDates(tokens, numbers)
	connectives := extractConnectives(tokens)

	return Analysis{
		Input:       input,
		Normalized:  normalized,
		Tokens:      tokens,
		Numbers:     numbers,
		Dates:       dates,
		Connectives: connectives,
		Spans:       buildSpans(normalized, tokens, numbers, dates, opts),
	}
}

func extractConnectives(tokens []Token) []Connective {
	var out []Connective
	for i, t := range tokens {
		if t.Kind != KindWord {
			continue
		}
		if op, ok := connectiveWords[t.Norm]; ok {
			out = append(out, Connective{Op: op, Text: t.Text, Start: i, End: i + 1})
		}
	}
	return out
}

// buildSpans produces the candidate set: every contiguous 1..MaxSpanTokens run
// of tokens, plus a typed span for each number and date.
//
// OVERLAPPING BY DESIGN. Phase 3 does not choose between readings. Segmenting
// greedily would be cheaper, but a wrong early split silently removes the
// correct interpretation before ranking has seen it — and Phase 2's misses
// (`client location` retrieving the customer entity, `recent customer`
// retrieving nothing useful) are exactly the case where the right split is not
// obvious without the config. Keeping every reading and letting Phase 4 pick a
// cover is the decision recorded in docs/decisions/010-span-generation.md.
func buildSpans(normalized string, tokens []Token, numbers []Number, dates []DateExpr, opts Options) []Span {
	// Tokens already claimed by a date are not emitted as text spans. "last 6
	// months" as an n-gram would embed to nothing useful and compete with the
	// date reading for no benefit. Numbers are NOT claimed the same way: a
	// number can legitimately be part of a phrase the ranker wants to see.
	claimed := make([]bool, len(tokens))
	for _, d := range dates {
		for i := d.Start; i < d.End && i < len(tokens); i++ {
			claimed[i] = true
		}
	}

	var spans []Span

	for _, d := range dates {
		dd := d
		spans = append(spans, Span{
			Kind:      SpanDate,
			Text:      d.Text,
			Norm:      FoldCase(d.Text),
			Start:     d.Start,
			End:       d.End,
			ByteStart: tokens[d.Start].Start,
			ByteEnd:   tokens[minIndex(d.End, len(tokens))-1].End,
			Date:      &dd,
		})
	}

	for _, n := range numbers {
		if n.Start < len(claimed) && claimed[n.Start] {
			continue // already part of a date window
		}
		nn := n
		spans = append(spans, Span{
			Kind:      SpanNumber,
			Text:      n.Text,
			Norm:      FoldCase(n.Text),
			Start:     n.Start,
			End:       n.End,
			ByteStart: tokens[n.Start].Start,
			ByteEnd:   tokens[minIndex(n.End, len(tokens))-1].End,
			Number:    &nn,
		})
	}

	// A text span covering exactly the same tokens as a number span duplicates
	// it and is worse than useless: embedding "50000" or "fifty thousand"
	// retrieves noise, while the typed span already carries the parsed value.
	// Spans that merely CONTAIN a number ("more than 50000") are still emitted,
	// because the surrounding words are what carry the operator.
	type tokenRange struct{ start, end int }
	numberRange := make(map[tokenRange]bool, len(numbers))
	for _, n := range numbers {
		numberRange[tokenRange{n.Start, n.End}] = true
	}

	for start := 0; start < len(tokens); start++ {
		if tokens[start].Kind == KindPunct || claimed[start] {
			continue
		}
		for length := 1; length <= opts.MaxSpanTokens && start+length <= len(tokens); length++ {
			end := start + length
			last := tokens[end-1]

			// A span may not end on punctuation, and may not run through it:
			// "customers, orders" is two phrases, not one.
			if last.Kind == KindPunct {
				break
			}
			if claimed[end-1] {
				break
			}

			allStop := true
			for i := start; i < end; i++ {
				if !tokens[i].Stop {
					allStop = false
					break
				}
			}
			if allStop {
				// Nothing but function words can never match a schema object,
				// and emitting it only gives the ranker noise to reject.
				continue
			}
			if numberRange[tokenRange{start, end}] {
				continue
			}

			spans = append(spans, Span{
				Kind:      SpanText,
				Text:      normalized[tokens[start].Start:last.End],
				Norm:      FoldCase(normalized[tokens[start].Start:last.End]),
				Start:     start,
				End:       end,
				ByteStart: tokens[start].Start,
				ByteEnd:   last.End,
			})
		}
	}

	sortSpans(spans)
	return spans
}

// sortSpans orders longest-first, then left-to-right, then by kind.
//
// Deterministic and specific-first: a caller taking the first acceptable match
// should see "more than" before "more". The kind tie-break keeps typed spans
// ahead of the text span covering the same tokens.
func sortSpans(spans []Span) {
	// Insertion sort: the candidate set is bounded by MaxSpans and typically
	// tens of entries, where this beats the overhead of anything cleverer and
	// is obviously stable.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spanLess(spans[j], spans[j-1]); j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

func spanLess(a, b Span) bool {
	al, bl := a.End-a.Start, b.End-b.Start
	if al != bl {
		return al > bl
	}
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	return kindRank(a.Kind) < kindRank(b.Kind)
}

func kindRank(k SpanKind) int {
	switch k {
	case SpanDate:
		return 0
	case SpanNumber:
		return 1
	default:
		return 2
	}
}

func minIndex(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TextSpans returns only the untyped candidate phrases, which are what gets
// embedded and sent to retrieval.
func (a Analysis) TextSpans() []Span {
	var out []Span
	for _, s := range a.Spans {
		if s.Kind == SpanText {
			out = append(out, s)
		}
	}
	return out
}
