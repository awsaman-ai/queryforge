package nlp

import (
	"strings"

	"queryforge.local/v2experiment/internal/numeric"
)

// Number is a numeric literal found in the text.
type Number struct {
	// Value is the number EXACTLY as written (Phase 8 gate, decision 022 §2).
	// It was a float64 until then, and a float64 changed long literals
	// silently: "99999999999999999999999" became 1e23. Invalid (the zero
	// Decimal) when Malformed is set.
	Value numeric.Decimal
	// Text is the surface form, for reporting.
	Text string
	// Start, End are token indices, half-open.
	Start, End int
	// Currency is an ISO code or symbol when one was attached ("$", "USD").
	// Recorded, not acted on: the experiment generates queries and does not
	// convert money.
	Currency string
	// Percent marks a value written with a % sign.
	Percent bool
	// Spelled marks a number written in words. Worth distinguishing because it
	// is a weaker signal — "one customer" is a count, not usually a filter
	// value — and Phase 4 may want to score it differently.
	Spelled bool
	// Malformed marks a number token that is not in a form this parser reads
	// exactly: an exponent ("1e5"), a second decimal point ("1.2.3"), grouping
	// that is not en-US thousands ("1,5", "1.000,5"), or a trailing tag that is
	// neither a magnitude nor a currency ("12x", "30th"). Before the Phase 8
	// gate such a token was either DROPPED — it produced no number, and a
	// number token is not a word, so nothing downstream noticed it — or read
	// as its leading digits ("12x" as 12). It is now reported, so binding can
	// refuse it (VALUE_RESOLUTION_FAILED) instead of a query going out
	// without it or with a value the user did not write.
	Malformed bool
}

// smallWords maps number words below twenty plus the tens.
var smallWords = map[string]int64{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fourty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// scaleWords are multipliers. "hundred" is a multiplier within a group;
// thousand and above close a group and add to the running total.
var scaleWords = map[string]int64{
	"hundred": 100, "thousand": 1000, "million": 1_000_000, "billion": 1_000_000_000,
}

// suffixMultipliers handle "50k", "1.2m", "3bn" — how people actually write
// large round numbers. Each is a power of ten, applied by moving the decimal
// point (numeric.Decimal.Shift): "4.35k" is 4350 by construction. The float64
// product 4.35 × 1000 also rounds to 4350, but only because the rounding error
// happens to cancel; "12.3456789012345678m" does not survive it.
var suffixMultipliers = map[string]int{
	"k": 3, "m": 6, "mm": 6, "bn": 9, "b": 9, "t": 12,
}

// currencySymbols maps a leading symbol to an ISO code.
var currencySymbols = map[string]string{
	"$": "USD", "£": "GBP", "€": "EUR", "¥": "JPY", "₹": "INR",
}

// currencyCodes are codes that may follow or precede a number.
var currencyCodes = map[string]bool{
	"usd": true, "gbp": true, "eur": true, "jpy": true, "inr": true,
	"cad": true, "aud": true, "chf": true, "cny": true,
}

// ExtractNumbers finds every numeric literal, digit-written or spelled out.
//
// Spelled-out numbers are not a nicety: "customers spending over fifty
// thousand" is one of the brief's own manual-gate sentences (section 11), and a
// pipeline that only saw digits would drop the value and silently produce a
// query with a missing filter.
func ExtractNumbers(tokens []Token) []Number {
	var out []Number

	i := 0
	for i < len(tokens) {
		if n, next, ok := parseDigitNumber(tokens, i); ok {
			out = append(out, n)
			i = next
			continue
		}
		if n, next, ok := parseSpelledNumber(tokens, i); ok {
			out = append(out, n)
			i = next
			continue
		}
		i++
	}
	return out
}

// parseDigitNumber reads a number token plus what is written against it: a
// leading decimal point, a minus sign and a currency symbol before it, a
// percent sign or a currency code after it.
//
// Every KindNumber token produces a Number — a well-formed one, or one marked
// Malformed. None is dropped: a number the pipeline cannot read exactly must
// reach binding, which refuses it, rather than vanish from the reading.
func parseDigitNumber(tokens []Token, i int) (Number, int, bool) {
	if tokens[i].Kind != KindNumber {
		return Number{}, i, false
	}

	n := Number{Text: tokens[i].Text, Start: i, End: i + 1}
	raw := tokens[i].Norm

	// What is written against the number, nearest first. Each prefix counts
	// only when it touches the number (no space between) and is not itself
	// glued to a preceding word or number: the "-" of "top-5" and "10-20" is
	// a hyphen, and the "." of "customers.5" ends a word, but "-5", "(-5)",
	// ".5" and "-.5" are signed and fractional values. Before the Phase 8 gate
	// these marks were punctuation, and "more than -5" was sent as 5 and
	// ".5" as 5: the value changed without a word said.
	//
	// A currency symbol keeps the rule it has always had: the token before the
	// number, touching or not ("$50000", "$ 50000"), in either order with a
	// sign ("-$5", "$-5").
	neg, point := false, false
	for {
		p := n.Start - 1
		if p < 0 || tokens[p].Kind != KindPunct {
			break
		}
		if code, isSymbol := currencySymbols[tokens[p].Text]; isSymbol && n.Currency == "" {
			n.Currency = code
			n.Start = p
			continue
		}
		glued := p > 0 && tokens[p-1].End == tokens[p].Start &&
			(tokens[p-1].Kind == KindWord || tokens[p-1].Kind == KindNumber)
		if tokens[p].End != tokens[n.Start].Start || glued {
			break
		}
		if tokens[p].Text == "." && !point && !neg && n.Currency == "" && p == i-1 {
			// ".5": the point belongs to the digits, so it is read with them.
			point = true
			raw = "0." + raw
			n.Text = "." + n.Text
			n.Start = p
			continue
		}
		if tokens[p].Text == "-" && !neg {
			neg = true
			n.Text = "-" + n.Text
			n.Start = p
			continue
		}
		break
	}

	value, suffix, ok := parseNumericToken(raw)
	if ok && neg {
		value = value.Neg()
	}
	if !ok {
		n.Malformed = true
	} else {
		n.Value = value
		if suffix != "" {
			// Only a currency code may remain: parseNumericToken refuses any
			// other tag.
			n.Currency = strings.ToUpper(suffix)
		}
	}

	next := i + 1
	if next < len(tokens) {
		switch {
		case tokens[next].Kind == KindPunct && tokens[next].Text == "%":
			n.Percent = true
			n.End = next + 1
			next++
		case tokens[next].Kind == KindWord && currencyCodes[tokens[next].Norm]:
			n.Currency = strings.ToUpper(tokens[next].Norm)
			n.End = next + 1
			next++
		}
	}
	return n, next, true
}

// parseNumericToken reads a number token's text EXACTLY, or reports that it
// cannot (ok false). The accepted form, and nothing else:
//
//	digits              50000   007
//	en-US grouping      50,000  1,234,567     (1–3 digits, then groups of 3)
//	a decimal fraction  1.5     50,000.25     (digits only after the point)
//	then optionally ONE of
//	  a magnitude       50k  1.2m  3mm  2bn  2b  1t
//	  a currency code   50usd  (returned as suffix, for the caller to attach)
//
// Everything else is refused rather than read approximately. That includes
// an exponent ("1e5", which float parsing used to accept), a second point
// ("1.2.3"), European grouping ("1,5" and "1.000,5", which comma removal
// used to turn into 15 and 1.0005), and an unknown tag ("12x", "30th",
// which used to be read as their digits). A config-driven locale is a
// question for whenever a measured failure points at it, not a guess to make
// now.
func parseNumericToken(raw string) (value numeric.Decimal, suffix string, ok bool) {
	end := 0
	for end < len(raw) && (raw[end] >= '0' && raw[end] <= '9' || raw[end] == ',' || raw[end] == '.') {
		end++
	}
	body, tag := raw[:end], raw[end:]
	for i := 0; i < len(tag); i++ {
		if tag[i] < 'a' || tag[i] > 'z' {
			return numeric.Decimal{}, "", false // digits after letters: "1a2"
		}
	}

	intPart, fracPart, hasPoint := strings.Cut(body, ".")
	if hasPoint && (fracPart == "" || strings.ContainsAny(fracPart, ".,")) {
		return numeric.Decimal{}, "", false
	}
	if strings.Contains(intPart, ",") {
		groups := strings.Split(intPart, ",")
		if len(groups[0]) < 1 || len(groups[0]) > 3 {
			return numeric.Decimal{}, "", false
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return numeric.Decimal{}, "", false
			}
		}
		intPart = strings.Join(groups, "")
	}
	text := intPart
	if hasPoint {
		text += "." + fracPart
	}
	value, err := numeric.Parse(text)
	if err != nil {
		return numeric.Decimal{}, "", false
	}

	switch {
	case tag == "":
		return value, "", true
	case suffixMultipliers[tag] > 0:
		shifted, err := value.Shift(suffixMultipliers[tag])
		if err != nil {
			return numeric.Decimal{}, "", false
		}
		return shifted, "", true
	case currencyCodes[tag]:
		return value, tag, true
	}
	return numeric.Decimal{}, "", false
}

// parseSpelledNumber reads a run of number words: "fifty thousand",
// "two hundred and fifty", "twenty five", "six".
//
// English number words do not simply accumulate. "Twenty five" is 25 because a
// tens word may be followed by a units word, but "two three" is two separate
// numbers, not five — a naive sum would invent a value that appears nowhere in
// the sentence and silently bind it as a filter. The group state below is what
// enforces that: within a group, at most one tens word and at most one
// units/teens word, and a scale word closes the group and resets it.
//
// Scale words follow the same principle (Phase 8 gate, decision 022 §2). Until
// then "one billion billion" summed to 2000000000 and "two hundred hundred"
// multiplied to 20000: values nobody wrote. Now:
//
//   - "hundred" multiplies a group of 1 to 99, once per group;
//   - thousand, million and billion must come in strictly decreasing order,
//     each after a quantity ("two million three thousand");
//
// and a run that breaks either rule is not a number this parser reads. It is
// reported Malformed, covering the offending scale word, and binding refuses
// it — as it refuses "1.2.3". Ending the number early instead would still
// invent one: "one thousand two million" would stop at "million" holding
// 1000 + 2 = 1002.
//
// Arithmetic is on int64, exactly. The rules bound the largest number that can
// form below 10^13 ("ninety nine hundred ninety nine billion …"), far inside
// int64, so no step can overflow.
func parseSpelledNumber(tokens []Token, i int) (Number, int, bool) {
	start := i
	var total, group int64
	var consumed bool

	// Within the current group: whether a units-or-teens word and a tens word
	// have already been taken. A second "hundred" needs no flag of its own:
	// after "hundred" the group is at least 100, which the 1-to-99 rule
	// already refuses.
	var haveUnit, haveTen bool
	// lastScale is the previous closing scale word's value (0 before any):
	// the next one must be smaller.
	var lastScale int64
	// malformed marks a run that broke the scale rules.
	var malformed bool

	for i < len(tokens) {
		t := tokens[i]
		if t.Kind != KindWord {
			break
		}
		word := t.Norm

		if v, isSmall := smallWords[word]; isSmall {
			switch {
			case v < 10: // a unit: valid alone, or after a tens word
				if haveUnit {
					goto done
				}
				haveUnit = true
			case v < 20: // a teen: cannot combine with anything in its group
				if haveUnit || haveTen {
					goto done
				}
				haveUnit = true
			default: // a tens word
				if haveTen || haveUnit {
					goto done
				}
				haveTen = true
			}
			group += v
			consumed = true
			i++
			continue
		}

		if scale, isScale := scaleWords[word]; isScale {
			if !consumed {
				// A bare "hundred" or "thousand" with no quantity before it is
				// not a number in this context — it is a word in a sentence.
				break
			}
			if scale == 100 {
				if group < 1 || group > 99 {
					malformed = true
					i++
					goto done
				}
				group *= scale
				// "two hundred fifty": the units and tens after "hundred" are
				// a fresh pair within the same group.
				haveUnit, haveTen = false, false
				i++
				continue
			}
			if group == 0 || (lastScale != 0 && scale >= lastScale) {
				malformed = true
				i++
				goto done
			}
			total += group * scale
			group = 0
			lastScale = scale
			haveUnit, haveTen = false, false
			i++
			continue
		}

		// "two hundred and fifty": the conjunction continues the number only
		// when another number word follows.
		if word == "and" && consumed && i+1 < len(tokens) && isNumberWord(tokens[i+1]) {
			i++
			continue
		}
		break
	}

done:
	if !consumed {
		return Number{}, start, false
	}
	// A trailing "and" that the loop stepped over belongs to the sentence,
	// not the number ("two hundred and hundred" ends at "two hundred").
	for !malformed && i > start && tokens[i-1].Norm == "and" {
		i--
	}

	n := Number{
		Text:    Text(tokens[start:i]),
		Start:   start,
		End:     i,
		Spelled: true,
	}
	if malformed {
		n.Malformed = true
	} else {
		n.Value = numeric.FromInt64(total + group)
	}
	return n, i, true
}

func isNumberWord(t Token) bool {
	if t.Kind != KindWord {
		return false
	}
	if _, ok := smallWords[t.Norm]; ok {
		return true
	}
	_, ok := scaleWords[t.Norm]
	return ok
}
