package ranking

import (
	"fmt"
	"strings"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/qferr"
)

// VerdictError is the sentence-level refusal: at least one phrase is ambiguous,
// or nothing in the sentence was accepted.
//
// It follows config.ValidationError's shape so the CLI renders both through
// one path: Error() is the redacted default, ErrorRevealed() is debug-only, and
// Unwrap exposes the coded error the evaluation harness counts.
//
// WHAT IS REDACTED. The competing targets are structure and are always named —
// "field:order.amount, field:payment.amount" is exactly what a caller needs in
// order to rephrase. A competing VALUE entry is named with its value withheld.
// The phrase itself is a literal from the user's sentence, which decision 006
// puts in the data domain, so the default message locates it by token position
// and only --debug quotes it. An error message is the output most likely to be
// shipped to a log aggregator; the --rank report, which the user asked for and
// which echoes their own sentence as --analyze does, is not.
type VerdictError struct {
	coded *qferr.Error
	// items render each ambiguous phrase twice: redacted and revealed.
	items     []verdictItem
	withheld  int
	minMargin float64
}

type verdictItem struct {
	start, end int
	phrase     qferr.Sensitive
	targets    []string // DisplayTarget(false)
	revealed   []string // DisplayTarget(true)
	margin     float64
	// shared is a refusal by the shared-phrase rule (decision 019 §5): the
	// margin may have cleared the bar, so it is not quoted as the reason.
	shared bool
}

// Err returns the sentence-level refusal, or nil when every covered span was
// accepted.
func (r Result) Err() error {
	switch r.Verdict {
	case "":
		return nil
	case qferr.CodeNoSemanticMatch:
		return &VerdictError{coded: qferr.New(qferr.CodeNoSemanticMatch,
			"no phrase in the sentence matched a configured target with enough confidence to accept")}
	}

	v := &VerdictError{minMargin: r.Policy.MinMargin}
	for _, i := range r.Cover {
		s := r.Spans[i]
		if s.Decision != Ambiguous {
			continue
		}
		it := verdictItem{
			start:  s.Span.Start,
			end:    s.Span.End,
			phrase: qferr.Sensitive(s.Span.Text),
			margin: s.Margin,
			shared: s.SharedPhrase,
		}
		v.withheld++
		for _, j := range append([]int{s.Top}, s.Competitors...) {
			c := s.Candidates[j]
			it.targets = append(it.targets, c.DisplayTarget(false))
			it.revealed = append(it.revealed, c.DisplayTarget(true))
			if c.Entry.Kind == corpus.KindValueAlias {
				v.withheld++
			}
		}
		v.items = append(v.items, it)
	}
	v.coded = qferr.New(qferr.CodeAmbiguousMatch, "%s", v.render(false))
	return v
}

func (v *VerdictError) render(reveal bool) string {
	var parts []string
	for _, it := range v.items {
		where := fmt.Sprintf("the phrase at tokens[%d:%d]", it.start, it.end)
		targets := it.targets
		if reveal {
			where = fmt.Sprintf("the phrase %q", it.phrase.Reveal())
			targets = it.revealed
		}
		if it.shared {
			// The first version of the shared-phrase rule reused the margin
			// message below and printed "(margin 0.294 < 0.100)" for X03: a
			// false statement, since 0.294 clears the bar. The reason is the
			// config's vocabulary, and that is what the caller can act on.
			parts = append(parts, fmt.Sprintf(
				"%s is part of the configured phrases of several targets, and nothing in the sentence distinguishes them: %s (use a configured phrase of one, or configure this phrase)",
				where, strings.Join(targets, ", ")))
			continue
		}
		parts = append(parts, fmt.Sprintf(
			"%s matches multiple configured targets with insufficient confidence separation: %s (margin %.3f < %.3f)",
			where, strings.Join(targets, ", "), it.margin, v.minMargin))
	}
	return strings.Join(parts, "; ")
}

// Error is the redacted rendering.
func (v *VerdictError) Error() string { return v.coded.Error() }

// Code is the stable failure category.
func (v *VerdictError) Code() qferr.Code { return v.coded.Code }

// Unwrap exposes the coded error.
func (v *VerdictError) Unwrap() error { return v.coded }

// ErrorRevealed quotes the phrases and names value targets in full.
//
// DEBUG ONLY, like config.ValidationError.ErrorRevealed.
func (v *VerdictError) ErrorRevealed() string {
	if len(v.items) == 0 {
		return v.coded.Message
	}
	return v.render(true)
}

// RedactedValueCount reports how many phrases and value targets the default
// rendering withheld. Zero means the revealed rendering would add nothing.
func (v *VerdictError) RedactedValueCount() int { return v.withheld }
