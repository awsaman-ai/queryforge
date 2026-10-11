package nlp

import (
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/numeric"
)

func analyze(t *testing.T, input string) Analysis {
	t.Helper()
	return Analyze(input, DefaultOptions)
}

// spanTexts returns the candidate span surfaces as a set, for membership
// assertions that do not depend on ordering.
func spanTexts(a Analysis) map[string]bool {
	out := make(map[string]bool, len(a.Spans))
	for _, s := range a.Spans {
		out[s.Norm] = true
	}
	return out
}

// Control and format characters are built from code points rather than typed
// as literals. A literal zero-width space or BOM in source is invisible to
// every reviewer, which is exactly the property that makes it a bug magnet —
// writing normalize.go the other way produced a file the Go parser rejected
// for an "illegal byte order mark" nobody could see.
func runes(cp ...rune) string { return string(cp) }

const (
	rightQuote = rune(0x2019) // ’
	leftDquote = rune(0x201C) // “
	rgtDquote  = rune(0x201D) // ”
	enDash     = rune(0x2013) // –
	nbsp       = rune(0x00A0)
	zwsp       = rune(0x200B)
	zwnj       = rune(0x200C)
	zwj        = rune(0x200D)
	softHyphen = rune(0x00AD)
	bom        = rune(0xFEFF)
)

func TestNormalizeFoldsTypography(t *testing.T) {
	cases := []struct{ in, want string }{
		{"customers" + runes(rightQuote) + " orders", "customers' orders"},
		{runes(leftDquote) + "spent" + runes(rgtDquote), `"spent"`},
		{"last" + runes(nbsp) + "6" + runes(nbsp) + "months", "last 6 months"},
		{"more" + runes(enDash) + "than", "more-than"},
		{"a" + runes(zwsp) + "b", "ab"}, // removed, not turned into a space
		{"  lots   of   space  ", "lots of space"},
		{"custo" + runes(softHyphen) + "mer", "customer"},
		{runes(bom) + "customers", "customers"},
	}
	for _, tc := range cases {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Normalization must not case-fold: the original casing survives on the token
// so a later phase can use it as evidence, and dropping it here would be
// irreversible.
func TestNormalizeKeepsCase(t *testing.T) {
	if got := Normalize("Customers From India"); got != "Customers From India" {
		t.Errorf("Normalize lowercased: %q", got)
	}
	a := analyze(t, "Customers From India")
	for _, tok := range a.Tokens {
		if tok.Text != "Customers" && tok.Text != "From" && tok.Text != "India" {
			t.Errorf("token %q lost its casing", tok.Text)
		}
		if tok.Norm != strings.ToLower(tok.Text) {
			t.Errorf("token %q has Norm %q", tok.Text, tok.Norm)
		}
	}
}

func TestTokenizeKinds(t *testing.T) {
	a := analyze(t, "customers spent 50,000.50 on 2 items, right?")

	var kinds []TokenKind
	for _, tok := range a.Tokens {
		kinds = append(kinds, tok.Kind)
	}
	want := []TokenKind{
		KindWord, KindWord, KindNumber, KindWord, KindNumber, KindWord,
		KindPunct, KindWord, KindPunct,
	}
	if len(kinds) != len(want) {
		t.Fatalf("got %d tokens %v, want %d", len(kinds), kinds, len(want))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("token %d (%q) kind = %s, want %s", i, a.Tokens[i].Text, kinds[i], want[i])
		}
	}
}

// Byte offsets must survive multi-byte runes, or a reported span points at the
// wrong part of the sentence — which is exactly when someone is trying to
// understand a rejection.
func TestTokenOffsetsSurviveMultibyte(t *testing.T) {
	a := analyze(t, "café customers spent 500")

	for _, tok := range a.Tokens {
		if got := a.Normalized[tok.Start:tok.End]; got != tok.Text {
			t.Errorf("token %d offsets [%d:%d] give %q, want %q", tok.Index, tok.Start, tok.End, got, tok.Text)
		}
	}
}

func TestWordInternalPunctuation(t *testing.T) {
	a := analyze(t, "customer's well-known e-mail")
	var words []string
	for _, tok := range a.Tokens {
		if tok.Kind == KindWord {
			words = append(words, tok.Norm)
		}
	}
	want := []string{"customer's", "well-known", "e-mail"}
	if len(words) != len(want) {
		t.Fatalf("got %v, want %v", words, want)
	}
	for i := range want {
		if words[i] != want[i] {
			t.Errorf("word %d = %q, want %q", i, words[i], want[i])
		}
	}
}

// A trailing period must not be swallowed into the number, or "spent 50." binds
// a different value than the user wrote.
func TestNumberDoesNotSwallowTrailingPunctuation(t *testing.T) {
	a := analyze(t, "spent 50.")
	if len(a.Numbers) != 1 {
		t.Fatalf("got %d numbers, want 1", len(a.Numbers))
	}
	if a.Numbers[0].Value.String() != "50" {
		t.Errorf("value = %v, want 50", a.Numbers[0].Value)
	}
}

func TestExtractNumbers(t *testing.T) {
	cases := []struct {
		input    string
		value    string
		currency string
		percent  bool
		spelled  bool
	}{
		{"more than 50000", "50000", "", false, false},
		{"more than 50,000", "50000", "", false, false},
		{"more than 1.5", "1.5", "", false, false},
		{"more than 50k", "50000", "", false, false},
		{"more than 1.2m", "1200000", "", false, false},
		{"more than $50000", "50000", "USD", false, false},
		{"more than 50000 USD", "50000", "USD", false, false},
		{"more than 20%", "20", "", true, false},
		{"over fifty thousand", "50000", "", false, true},
		{"over two hundred", "200", "", false, true},
		{"over two hundred and fifty", "250", "", false, true},
		{"exactly six", "6", "", false, true},
		{"one million", "1000000", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			a := analyze(t, tc.input)
			if len(a.Numbers) != 1 {
				t.Fatalf("got %d numbers %+v, want 1", len(a.Numbers), a.Numbers)
			}
			n := a.Numbers[0]
			if n.Value.String() != tc.value {
				t.Errorf("value = %v, want %v", n.Value, tc.value)
			}
			if n.Currency != tc.currency {
				t.Errorf("currency = %q, want %q", n.Currency, tc.currency)
			}
			if n.Percent != tc.percent {
				t.Errorf("percent = %v, want %v", n.Percent, tc.percent)
			}
			if n.Spelled != tc.spelled {
				t.Errorf("spelled = %v, want %v", n.Spelled, tc.spelled)
			}
		})
	}
}

// English number words do not simply accumulate. A greedy sum turned
// "one two three" into 6 — a value that appears nowhere in the sentence and
// would have been bound as a filter.
func TestSpelledNumberGrouping(t *testing.T) {
	cases := []struct {
		input  string
		values []string
	}{
		{"twenty five customers", []string{"25"}},
		{"one hundred and twenty five", []string{"125"}},
		{"fifty thousand", []string{"50000"}},
		{"two hundred and fifty", []string{"250"}},
		// Separate numbers, not one sum.
		{"one two three", []string{"1", "2", "3"}},
		{"six seven", []string{"6", "7"}},
		// A teen cannot combine with anything in its group.
		{"thirteen fourteen", []string{"13", "14"}},
		{"twenty thirty", []string{"20", "30"}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			a := analyze(t, tc.input)
			if len(a.Numbers) != len(tc.values) {
				t.Fatalf("got %d numbers %+v, want %d", len(a.Numbers), a.Numbers, len(tc.values))
			}
			for i, want := range tc.values {
				if a.Numbers[i].Value.String() != want {
					t.Errorf("number %d = %v, want %v", i, a.Numbers[i].Value, want)
				}
			}
		})
	}
}

// A bare scale word is a word in a sentence, not a number. Without this,
// "thousands of customers" would parse a value of 1000 and invent a filter.
func TestBareScaleWordIsNotANumber(t *testing.T) {
	for _, input := range []string{"thousands of customers", "hundreds of orders", "million dollar question"} {
		a := analyze(t, input)
		for _, n := range a.Numbers {
			if n.Spelled && n.Value.Cmp(numeric.FromInt64(100)) >= 0 {
				t.Errorf("%q produced a spurious number %v", input, n.Value)
			}
		}
	}
}

func TestExtractDates(t *testing.T) {
	cases := []struct {
		input  string
		unit   DateUnit
		anchor DateAnchor
		count  int
		offset int
	}{
		{"created last year", UnitYear, AnchorCalendar, 1, -1},
		{"created last month", UnitMonth, AnchorCalendar, 1, -1},
		{"this month", UnitMonth, AnchorCalendar, 1, 0},
		{"this year", UnitYear, AnchorCalendar, 1, 0},
		{"in the last 6 months", UnitMonth, AnchorRolling, 6, 0},
		{"in the last six months", UnitMonth, AnchorRolling, 6, 0},
		{"past 30 days", UnitDay, AnchorRolling, 30, 0},
		{"previous 2 weeks", UnitWeek, AnchorRolling, 2, 0},
		{"last quarter", UnitQuarter, AnchorCalendar, 1, -1},
		{"today", UnitDay, AnchorCalendar, 1, 0},
		{"yesterday", UnitDay, AnchorCalendar, 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			a := analyze(t, tc.input)
			if len(a.Dates) != 1 {
				t.Fatalf("got %d dates %+v, want 1", len(a.Dates), a.Dates)
			}
			d := a.Dates[0]
			if d.Unit != tc.unit || d.Anchor != tc.anchor || d.Count != tc.count || d.Offset != tc.offset {
				t.Errorf("got {unit:%s anchor:%s count:%d offset:%d}, want {unit:%s anchor:%s count:%d offset:%d}",
					d.Unit, d.Anchor, d.Count, d.Offset, tc.unit, tc.anchor, tc.count, tc.offset)
			}
		})
	}
}

// "Last 6 months" is the six months up to now; "last month" is the previous
// calendar month. Collapsing the two shifts every boundary by up to a month,
// and the error is invisible in the generated SQL.
func TestRollingVersusCalendarDistinction(t *testing.T) {
	rolling := analyze(t, "last 6 months")
	if len(rolling.Dates) != 1 || rolling.Dates[0].Anchor != AnchorRolling {
		t.Errorf("an explicit count should give a rolling window, got %+v", rolling.Dates)
	}
	calendar := analyze(t, "last month")
	if len(calendar.Dates) != 1 || calendar.Dates[0].Anchor != AnchorCalendar {
		t.Errorf("a bare period should give a calendar window, got %+v", calendar.Dates)
	}
}

// Without a plausibility bound, "spent more than 50000" would read the value as
// a year and lose the filter entirely. Inside the bound both readings survive
// and Phase 4, which has the field types, chooses.
func TestYearPlausibilityBound(t *testing.T) {
	inRange := analyze(t, "spent more than 2000")
	if len(inRange.Numbers) == 0 {
		t.Error("the numeric reading must survive alongside any year reading")
	}

	outOfRange := analyze(t, "spent more than 50000")
	if len(outOfRange.Dates) != 0 {
		t.Errorf("50000 is not a plausible year, got %+v", outOfRange.Dates)
	}
	if len(outOfRange.Numbers) != 1 {
		t.Errorf("got %d numbers, want 1", len(outOfRange.Numbers))
	}
}

func TestAbsoluteDates(t *testing.T) {
	a := analyze(t, "orders in January 2024")
	if len(a.Dates) != 1 {
		t.Fatalf("got %d dates %+v, want 1", len(a.Dates), a.Dates)
	}
	d := a.Dates[0]
	if d.Anchor != AnchorAbsolute || d.Year != 2024 || int(d.Month) != 1 {
		t.Errorf("got %+v, want absolute January 2024", d)
	}

	// A month with no year is ambiguous; guessing the current year would be a
	// silent assumption, so it stays an ordinary span.
	b := analyze(t, "orders in January")
	if len(b.Dates) != 0 {
		t.Errorf("a bare month should not resolve to a date: %+v", b.Dates)
	}
}

func TestConnectives(t *testing.T) {
	a := analyze(t, "customers from India and orders above 100 or below 10")
	var ops []string
	for _, c := range a.Connectives {
		ops = append(ops, c.Op)
	}
	if len(ops) != 2 || ops[0] != "and" || ops[1] != "or" {
		t.Errorf("connectives = %v, want [and or]", ops)
	}

	b := analyze(t, "customers excluding India")
	if len(b.Connectives) != 1 || b.Connectives[0].Op != "not" {
		t.Errorf("connectives = %+v, want one not", b.Connectives)
	}
}

func TestSpansAreDeterministic(t *testing.T) {
	const input = "show the top 10 customers from India who spent more than 50000 last year"
	first := analyze(t, input)

	for i := 0; i < 20; i++ {
		again := analyze(t, input)
		if len(again.Spans) != len(first.Spans) {
			t.Fatalf("span count varies: %d then %d", len(first.Spans), len(again.Spans))
		}
		for j := range again.Spans {
			if again.Spans[j].Text != first.Spans[j].Text || again.Spans[j].Start != first.Spans[j].Start {
				t.Fatalf("span %d varies: %+v then %+v", j, first.Spans[j], again.Spans[j])
			}
		}
	}
}

func TestSpansAreOrderedLongestFirst(t *testing.T) {
	a := analyze(t, "customers who spent more than 50000")
	for i := 1; i < len(a.Spans); i++ {
		prev := a.Spans[i-1].End - a.Spans[i-1].Start
		cur := a.Spans[i].End - a.Spans[i].Start
		if cur > prev {
			t.Fatalf("span %d is longer than its predecessor: %q then %q",
				i, a.Spans[i-1].Text, a.Spans[i].Text)
		}
	}
}

// The overlapping strategy: every reading survives to Phase 4. "more than" must
// be a candidate even though "more", "than" and "spent more than" also are.
func TestOverlappingSpansAllSurvive(t *testing.T) {
	texts := spanTexts(analyze(t, "customers who spent more than 50000"))
	for _, want := range []string{"customers", "spent", "more", "than", "more than", "spent more than"} {
		if !texts[want] {
			t.Errorf("expected a candidate span %q", want)
		}
	}
}

// Function words are marked, never removed — "more than" depends on one — but a
// span of nothing but function words can never match a schema object.
func TestAllStopSpansAreDropped(t *testing.T) {
	texts := spanTexts(analyze(t, "show me the customers"))
	for _, unwanted := range []string{"show", "me", "the", "show me", "show me the"} {
		if texts[unwanted] {
			t.Errorf("a span of only function words should be dropped: %q", unwanted)
		}
	}
	if !texts["customers"] {
		t.Error("the content word should survive")
	}
}

// A span may not run through punctuation: "customers, orders" is two phrases.
func TestSpansDoNotCrossPunctuation(t *testing.T) {
	for span := range spanTexts(analyze(t, "customers, orders")) {
		if strings.Contains(span, ",") {
			t.Errorf("span %q crosses punctuation", span)
		}
	}
}

func TestMaxSpanTokensIsRespected(t *testing.T) {
	a := Analyze("customers orders products invoices shipments returns", Options{MaxSpanTokens: 2})
	for _, s := range a.Spans {
		if s.End-s.Start > 2 {
			t.Errorf("span %q is %d tokens, over the limit of 2", s.Text, s.End-s.Start)
		}
	}
}

// Numbers and dates get typed spans; a text span covering exactly the same
// tokens is suppressed because embedding a bare numeral retrieves noise.
func TestTypedSpansDoNotDuplicateTextSpans(t *testing.T) {
	a := analyze(t, "spent more than 50000")
	var numberSpans, textDuplicates int
	for _, s := range a.Spans {
		switch {
		case s.Kind == SpanNumber:
			numberSpans++
		case s.Kind == SpanText && s.Norm == "50000":
			textDuplicates++
		}
	}
	if numberSpans != 1 {
		t.Errorf("got %d number spans, want 1", numberSpans)
	}
	if textDuplicates != 0 {
		t.Errorf("got %d duplicate text spans for the number", textDuplicates)
	}

	// But a span containing the number survives: the words around it carry the
	// operator.
	if !spanTexts(a)["more than 50000"] {
		t.Error("a multi-token span containing the number should survive")
	}
}

func TestDateTokensAreNotAlsoTextSpans(t *testing.T) {
	a := analyze(t, "customers created last year")
	for _, s := range a.Spans {
		if s.Kind != SpanText {
			continue
		}
		if strings.Contains(s.Norm, "last year") {
			t.Errorf("date tokens should not also be text spans: %q", s.Text)
		}
	}
}

// Robustness, from section 21: the system must not panic, whatever arrives.
func TestAnalyzeDoesNotPanic(t *testing.T) {
	inputs := []string{
		"", " ", "\n\t ", "?", "!!!", "...",
		strings.Repeat("a", 10000),
		strings.Repeat("customers ", 500),
		runes(0, 1, 2),
		"日本語のテキスト",
		"émojis 🙂 and ünïcödé",
		"50000 50000 50000",
		"last last last months months",
		"--config /etc/passwd",
		"'; DROP TABLE users; --",
		"1e999999",
		"....50000....",
		runes(bom, zwsp, zwnj, zwj),
		"last " + strings.Repeat("9", 400) + " months",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Analyze(%q) panicked: %v", truncateForLog(in), r)
				}
			}()
			a := analyze(t, in)
			// Every span's offsets must be valid into the normalised string.
			for _, s := range a.Spans {
				if s.ByteStart < 0 || s.ByteEnd > len(a.Normalized) || s.ByteStart > s.ByteEnd {
					t.Errorf("Analyze(%q) produced an invalid span offset %d:%d",
						truncateForLog(in), s.ByteStart, s.ByteEnd)
				}
			}
		}()
	}
}

func truncateForLog(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func TestEmptyInputProducesNothing(t *testing.T) {
	a := analyze(t, "   ")
	if len(a.Tokens) != 0 || len(a.Spans) != 0 {
		t.Errorf("blank input produced %d tokens and %d spans", len(a.Tokens), len(a.Spans))
	}
}
