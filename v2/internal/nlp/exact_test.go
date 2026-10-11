package nlp

import (
	"testing"
)

// Phase 8 gate, decision 022 §2: a number reaches the rest of the pipeline
// exactly as written, or marked Malformed so binding refuses it. It is never
// rounded, truncated, re-signed or dropped.

// oneNumber analyses input and returns its only number.
func oneNumber(t *testing.T, input string) Number {
	t.Helper()
	a := analyze(t, input)
	if len(a.Numbers) != 1 {
		t.Fatalf("%q: got %d numbers %+v, want 1", input, len(a.Numbers), a.Numbers)
	}
	return a.Numbers[0]
}

// The finding (V02): a 23-digit literal came out as 1e23.
func TestLargeIntegersAreExact(t *testing.T) {
	for _, lit := range []string{
		"99999999999999999999999",
		"9007199254740993", // 2^53 + 1: the first integer float64 cannot hold
		"123456789012345678901234567890123456789",
	} {
		n := oneNumber(t, "more than "+lit)
		if n.Malformed || n.Value.String() != lit {
			t.Errorf("%s: value %s, malformed %v", lit, n.Value, n.Malformed)
		}
	}
}

func TestHighPrecisionDecimalsAreExact(t *testing.T) {
	for _, lit := range []string{"0.1234567890123456789", "12345678901234567.25", "3.14159265358979323846"} {
		n := oneNumber(t, "more than "+lit)
		if n.Malformed || n.Value.String() != lit {
			t.Errorf("%s: value %s, malformed %v", lit, n.Value, n.Malformed)
		}
	}
}

// Magnitude suffixes move the decimal point; no float product.
func TestSuffixesAreExact(t *testing.T) {
	cases := map[string]string{
		"4.35k":                "4350", // exact by construction, not by float rounding luck
		"0.1k":                 "100",
		"1.2m":                 "1200000",
		"2.5bn":                "2500000000",
		"1.5t":                 "1500000000000",
		"50,000k":              "50000000",
		"12.3456789012345678m": "12345678.9012345678",
	}
	for in, want := range cases {
		n := oneNumber(t, "over "+in)
		if n.Malformed || n.Value.String() != want {
			t.Errorf("%s: value %s, malformed %v; want %s", in, n.Value, n.Malformed, want)
		}
	}
}

// Before the gate the sign was punctuation: "-5" was sent as 5.
func TestSignIsPartOfTheNumber(t *testing.T) {
	cases := map[string]string{
		"more than -5":    "-5",
		"more than -5.25": "-5.25",
		"more than (-5)":  "-5",
		"more than -.5":   "-0.5",
		"more than -5k":   "-5000",
	}
	for in, want := range cases {
		n := oneNumber(t, in)
		if n.Malformed || n.Value.String() != want {
			t.Errorf("%q: value %s; want %s", in, n.Value, want)
		}
	}
	// A hyphen glued to a word or a number is not a sign.
	for in, want := range map[string]string{"top-5 customers": "5", "customers-5": "5"} {
		n := oneNumber(t, in)
		if n.Value.String() != want {
			t.Errorf("%q: value %s; want %s (a hyphen, not a sign)", in, n.Value, want)
		}
	}
	// A spaced dash is punctuation, as before.
	if n := oneNumber(t, "customers - 5"); n.Value.String() != "5" {
		t.Errorf("spaced dash: value %s", n.Value)
	}
}

// ".5" was read as 5.
func TestLeadingDecimalPoint(t *testing.T) {
	for in, want := range map[string]string{"more than .5": "0.5", "above .25k": "250"} {
		n := oneNumber(t, in)
		if n.Malformed || n.Value.String() != want {
			t.Errorf("%q: value %s; want %s", in, n.Value, want)
		}
	}
	// A point that ends a word is not a decimal point.
	if n := oneNumber(t, "customers.5"); n.Value.String() != "5" {
		t.Errorf("customers.5: value %s", n.Value)
	}
}

// Currency and percent attach exactly as before, with the exact value.
func TestCurrencyAndPercentKeepTheirRule(t *testing.T) {
	cases := []struct {
		in, value, currency string
		percent             bool
	}{
		{"$50000", "50000", "USD", false},
		{"$ 50000", "50000", "USD", false}, // a space never dropped the symbol
		{"-$5", "-5", "USD", false},
		{"$-5", "-5", "USD", false},
		{"50000 usd", "50000", "USD", false},
		{"50usd", "50", "USD", false},
		{"12.5%", "12.5", "", true},
	}
	for _, c := range cases {
		n := oneNumber(t, "more than "+c.in)
		if n.Malformed || n.Value.String() != c.value || n.Currency != c.currency || n.Percent != c.percent {
			t.Errorf("%q: %+v", c.in, n)
		}
	}
}

// Every number token that is not in the accepted form is MARKED, never
// dropped and never read as its leading digits.
func TestMalformedNumbersAreReported(t *testing.T) {
	for _, in := range []string{
		"1e5",     // exponent: float parsing used to accept it
		"1e309",   // … and this one it dropped (out of float range)
		"1.2.3",   // two points: dropped
		"1,5",     // European decimal: read as 15
		"1.000,5", // European grouping: read as 1.0005
		"12,34",   // bad group: read as 1234
		"1234,567,890",
		"12x",   // unknown tag: read as 12
		"30th",  // ordinal: read as 30
		"1a2",   // digits after letters: dropped
		"5kusd", // two tags
	} {
		n := oneNumber(t, "more than "+in)
		if !n.Malformed {
			t.Errorf("%q: read as %s, want Malformed", in, n.Value)
		}
		if n.Value.Valid() {
			t.Errorf("%q: a malformed number carries a value (%s)", in, n.Value)
		}
	}
	// Well-formed grouping still reads.
	for in, want := range map[string]string{"1,234,567": "1234567", "50,000.25": "50000.25", "007": "7"} {
		if n := oneNumber(t, "more than "+in); n.Malformed || n.Value.String() != want {
			t.Errorf("%q: %+v, want %s", in, n, want)
		}
	}
}

// Spelled numbers: scale words may not repeat or rise. Before the gate "one
// billion billion" summed to 2000000000; a run that breaks the rules is now
// Malformed (refused at binding), never a partial value — ending it early
// would read "one thousand two million" as 1002.
func TestSpelledScalesDoNotInventValues(t *testing.T) {
	for _, in := range []string{
		"one billion billion",
		"two hundred hundred",
		"one thousand thousand",
		"two thousand million",
		"two million thousand",
		"one thousand two million",
		"one hundred twenty hundred",
		"two hundred three hundred",
	} {
		a := analyze(t, in)
		if len(a.Numbers) != 1 || !a.Numbers[0].Malformed || a.Numbers[0].Value.Valid() {
			t.Errorf("%q: numbers %+v, want one Malformed run", in, a.Numbers)
			continue
		}
		if n := a.Numbers[0]; n.Start != 0 || n.End != len(a.Tokens) {
			t.Errorf("%q: the malformed run covers tokens[%d:%d], want the whole run", in, n.Start, n.End)
		}
	}
	// The ordinary forms are unchanged.
	for in, want := range map[string]string{
		"two million three thousand four hundred and five": "2003405",
		"nineteen hundred":     "1900",
		"twenty five hundred":  "2500",
		"one hundred thousand": "100000",
		"nine hundred ninety nine billion nine hundred ninety nine million nine hundred ninety nine thousand nine hundred ninety nine": "999999999999",
	} {
		a := analyze(t, in)
		if len(a.Numbers) != 1 || a.Numbers[0].Malformed || a.Numbers[0].Value.String() != want {
			t.Errorf("%q: numbers %+v, want %s", in, a.Numbers, want)
		}
	}
	// Separate numbers stay separate, as before (TestSpelledNumberGrouping).
	if a := analyze(t, "one two three"); len(a.Numbers) != 3 {
		t.Errorf("one two three: %+v", a.Numbers)
	}
}

// A window count is a whole number of at least one, or no window forms.
// Before the gate "last 2.5 months" was last 2 months and "last 0 days" was
// last 1 day.
func TestWindowCountIsExact(t *testing.T) {
	for _, in := range []string{
		"in the last 2.5 months",
		"in the last 0 days",
		"in the last -5 days",
		"in the last 99999999999999999999999 days",
		"in the last 1e3 days",
		"in the last $5 days",
	} {
		a := analyze(t, in)
		for _, d := range a.Dates {
			if d.Anchor == AnchorRolling || d.Count != 1 || d.Unit == UnitDay {
				t.Errorf("%q formed a window %+v from a count that is not a whole number of periods", in, d)
			}
		}
	}
	a := analyze(t, "in the last 6 months")
	if len(a.Dates) != 1 || a.Dates[0].Count != 6 || a.Dates[0].Anchor != AnchorRolling {
		t.Errorf("ordinary window: %+v", a.Dates)
	}
	a = analyze(t, "in the last 2147483647 days")
	if len(a.Dates) != 1 || a.Dates[0].Count != maxWindowCount {
		t.Errorf("largest count: %+v", a.Dates)
	}
	a = analyze(t, "in the last 2147483648 days")
	for _, d := range a.Dates {
		if d.Unit == UnitDay {
			t.Errorf("a count over maxWindowCount formed a window: %+v", d)
		}
	}
}

// The request verbs are a subset of the function words, and each is one.
func TestRequestVerbsAreStopWords(t *testing.T) {
	for v := range requestVerbs {
		if !stopWords[v] {
			t.Errorf("request verb %q is not a stop word", v)
		}
		if !IsRequestVerb(v) {
			t.Errorf("IsRequestVerb(%q) = false", v)
		}
	}
	for _, w := range []string{"the", "of", "please", "want", "customers", ""} {
		if IsRequestVerb(w) {
			t.Errorf("IsRequestVerb(%q) = true", w)
		}
	}
}
