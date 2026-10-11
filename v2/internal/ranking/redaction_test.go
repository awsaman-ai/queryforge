package ranking

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/qferr"
)

// The redaction contract (decision 006) as it applies to ranking. Ranking is a
// new place for values to escape: its candidates include value entries, whose
// IDs EMBED the value and whose matched surface IS the value, and its
// diagnostics print targets, surfaces and reasons for every candidate.

// configValues returns every value in a fixture: enum values, alias keys and
// alias targets. Read from the registry, not listed by hand, so a value added
// to the fixture is covered without editing this test.
func configValues(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, e := range corpus.Build(loadRegistry(t, name)) {
		if e.Kind != corpus.KindValueAlias {
			continue
		}
		for _, s := range e.Surfaces {
			out = append(out, s.Text)
		}
		out = append(out, e.Meta.ValueTarget)
	}
	return out
}

// A value candidate renders redacted by default and in full on reveal, for
// both its target and its matched surface.
func TestValueCandidateRendersRedacted(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "zorbified", map[string][]hit{
		"zorbified": {{"value:customer.status:alias:zorbified", 1.0, "zorbified"}},
	}), DefaultPolicy())
	c, _ := spanNamed(t, res, "zorbified").TopCandidate()

	if got := c.DisplayTarget(false); strings.Contains(got, "zorbified") || !strings.Contains(got, qferr.Placeholder) {
		t.Errorf("DisplayTarget(false) = %q", got)
	}
	if got := c.MatchedText(false); got != qferr.Placeholder {
		t.Errorf("MatchedText(false) = %q", got)
	}
	if got := c.DisplayTarget(true); !strings.Contains(got, "zorbified") {
		t.Errorf("DisplayTarget(true) = %q, want the value revealed", got)
	}
	if got := c.MatchedText(true); got != "zorbified" {
		t.Errorf("MatchedText(true) = %q", got)
	}
	// The stored value the alias resolves to never appears in either
	// rendering: nothing in ranking's output surface exposes it.
	for _, r := range []string{c.DisplayTarget(true), c.MatchedText(true)} {
		if strings.Contains(r, "lapsed_qx") {
			t.Errorf("rendering %q exposes the value target", r)
		}
	}
}

// Reasons are printed in default output, so they may name structure only.
// Swept against every value in the fixture, across sentences that retrieve
// value entries.
func TestReasonsNeverCarryValues(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	values := configValues(t, "ambiguity.json")
	for _, sentence := range []string{
		"customers from Freedonia with status zorbified",
		"payments that bounced over 5",
		"sylvania customers who placed an order",
	} {
		hits := map[string][]hit{}
		for _, w := range []string{"freedonia", "zorbified", "bounced", "sylvania"} {
			if strings.Contains(strings.ToLower(sentence), w) {
				hits[w] = []hit{
					{"value:customer.country:alias:Freedonia", 0.9, "Freedonia"},
					{"value:customer.country:alias:Sylvania", 0.9, "Sylvania"},
					{"value:customer.status:alias:zorbified", 0.8, "zorbified"},
					{"value:payment.status:alias:bounced", 0.8, "bounced"},
					{"field:customer.country", 0.7, "customer country"},
				}
			}
		}
		res := mustRank(t, script(t, reg, sentence, hits), DefaultPolicy())
		for _, s := range res.Spans {
			for _, c := range s.Candidates {
				for _, r := range c.Reasons {
					for _, v := range values {
						if v != "" && strings.Contains(r, v) {
							t.Errorf("%q: reason %q contains the config value %q", sentence, r, v)
						}
					}
				}
			}
		}
	}
}

// The sentence-level refusal: targets named, values and the phrase withheld;
// both revealed under the debug rendering.
func TestVerdictErrorRedactsAndReveals(t *testing.T) {
	// Two LITERAL readings of one word (decision 019: an absent value can no
	// longer compete), so the ambiguity names two value targets.
	reg := withSharedValue(t)
	res := mustRank(t, script(t, reg, "customers from Freedonia", map[string][]hit{
		"freedonia": {
			{"value:customer.country:alias:Freedonia", 0.9, "Freedonia"},
			{"value:payment.status:alias:Freedonia", 0.9, "Freedonia"},
		},
	}), DefaultPolicy())
	err := res.Err()
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	if qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Fatalf("code = %s", qferr.CodeOf(err))
	}

	def := err.Error()
	for _, leak := range []string{"Freedonia", "Sylvania", "FD_QX", "SY_QX"} {
		if strings.Contains(def, leak) {
			t.Errorf("default error contains %q: %s", leak, def)
		}
	}
	if !strings.Contains(def, "value:customer.country:alias:"+qferr.Placeholder) {
		t.Errorf("default error should still name the structure: %s", def)
	}
	if !strings.Contains(def, "tokens[2:3]") {
		t.Errorf("default error should locate the phrase by position: %s", def)
	}

	var ve *VerdictError
	if !errors.As(err, &ve) {
		t.Fatal("not a *VerdictError")
	}
	if ve.RedactedValueCount() == 0 {
		t.Error("RedactedValueCount = 0, but the phrase and two value targets were withheld")
	}
	rev := ve.ErrorRevealed()
	for _, want := range []string{`"Freedonia"`, "customer.country:alias:Freedonia", "payment.status:alias:Freedonia"} {
		if !strings.Contains(rev, want) {
			t.Errorf("revealed error lacks %q: %s", want, rev)
		}
	}
	// The coded error in the chain carries only the redacted message.
	var coded *qferr.Error
	if !errors.As(err, &coded) || strings.Contains(coded.Message, "Freedonia") {
		t.Errorf("coded message leaks: %+v", coded)
	}
}

// An ambiguity between two STRUCTURAL targets still withholds the phrase —
// it is user input — but reports nothing else as withheld.
func TestVerdictErrorOnStructuralTargets(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "amount", map[string][]hit{
		"amount": {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
	}), DefaultPolicy())
	var ve *VerdictError
	if !errors.As(res.Err(), &ve) {
		t.Fatal("expected a VerdictError")
	}
	if !strings.Contains(ve.Error(), "field:order.amount, field:payment.amount") {
		t.Errorf("targets not named: %s", ve.Error())
	}
	if ve.RedactedValueCount() != 1 {
		t.Errorf("withheld = %d, want 1 (the phrase)", ve.RedactedValueCount())
	}
	if !strings.Contains(ve.ErrorRevealed(), `"amount"`) {
		t.Errorf("revealed form should quote the phrase: %s", ve.ErrorRevealed())
	}
}

// The safe rendering is the one you get by accident: formatting a Candidate
// with any fmt verb — the obvious debugging mistake — redacts, because
// Candidate implements Stringer and GoStringer. Without them, %v would print
// the unexported matched surface and the Entry, whose ID embeds the value.
func TestFormattingACandidateRedacts(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "freedonia", map[string][]hit{
		"freedonia": {{"value:customer.country:alias:Freedonia", 1.0, "Freedonia"}},
	}), DefaultPolicy())
	c, _ := spanNamed(t, res, "freedonia").TopCandidate()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, c); strings.Contains(got, "Freedonia") || strings.Contains(got, "FD_QX") {
			t.Errorf("%s of a candidate leaks: %s", verb, got)
		}
	}
	// And inside a slice, as a debug print of a whole span would do.
	if got := fmt.Sprintf("%+v", []Candidate{c}); strings.Contains(got, "Freedonia") {
		t.Errorf("a slice of candidates leaks: %s", got)
	}
}
