// Package numeric holds the one representation of a number the user wrote:
// an exact decimal (Phase 8 gate, decision 022 §2).
//
// WHY NOT float64. Until the Phase 8 gate, numbers travelled from extraction
// to the query's parameters as float64. A float64 holds about 15–17
// significant decimal digits and nothing more, so a user's literal could
// change on the way through without anything saying so:
// "more than 99999999999999999999999" reached the parameters as 1e23 (finding
// V02). The query was a valid SELECT, and it answered a different question.
// That is the failure this experiment refuses everywhere else: a value the
// sentence did not say, sent as if it had.
//
// A Decimal is the value exactly as written: a sign, a run of decimal digits
// and a decimal point. There is no rounding anywhere in this package. An
// operation that cannot give an exact result (none is needed today) must not
// be added; an input that cannot be represented is an error, never an
// approximation.
//
// THE ZERO VALUE IS NOT A NUMBER. Decimal{} is invalid, and every consumer
// checks Valid. A plan value built by hand without a number is therefore
// refused by the compiler, instead of silently meaning zero.
//
// BOUNDS. The query's dialect is PostgreSQL, whose exact type (numeric) holds
// up to 131072 digits before the decimal point and 16383 after it. A number
// outside that range cannot be sent as the value it is, so it is refused.
// The bound also keeps a short input from expanding into a huge one: the
// JSON literal "1e999999999" is eleven bytes and would be a billion digits.
package numeric

import (
	"errors"
	"strconv"
	"strings"
)

// The limits of PostgreSQL's numeric type: the dialect the compiler emits.
const (
	MaxIntegerDigits  = 131072
	MaxFractionDigits = 16383
)

// ErrOutOfRange is returned for a number PostgreSQL's numeric cannot hold.
var ErrOutOfRange = errors.New("the number is outside the range of an exact PostgreSQL numeric (131072 digits before the decimal point, 16383 after)")

// ErrSyntax is returned for text that is not a number in the accepted form.
var ErrSyntax = errors.New("not a decimal number")

// Decimal is an exact base-10 number. The value is digits × 10^(−scale),
// negated when neg is set. The fields are kept canonical by every
// constructor, so two Decimals with the same value have identical fields and
// the same String:
//
//   - digits has no leading zero, except the single "0" of zero;
//   - when scale > 0, digits does not end in a zero (a trailing fractional
//     zero changes no value: 9.90 is 9.9);
//   - zero is never negative, and has scale 0.
type Decimal struct {
	neg    bool
	digits string
	scale  int
}

// Valid reports whether d holds a number. The zero Decimal does not.
func (d Decimal) Valid() bool { return d.digits != "" }

// Parse reads the canonical text form: an optional minus sign, one or more
// digits, and optionally a decimal point followed by one or more digits.
// Nothing else — no plus sign, no exponent, no grouping, no surrounding
// space, no leading or trailing decimal point — because every caller has
// already reduced its own notation (grouping commas, magnitude suffixes) to
// this form, and a wider grammar here would accept text no caller meant.
func Parse(s string) (Decimal, error) {
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	intPart, fracPart, hasPoint := strings.Cut(s, ".")
	if intPart == "" || !allDigits(intPart) || (hasPoint && (fracPart == "" || !allDigits(fracPart))) {
		return Decimal{}, ErrSyntax
	}
	return build(neg, intPart, fracPart)
}

// MustParse is Parse for constants and tests; it panics on bad input.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic("numeric.MustParse(" + strconv.Quote(s) + "): " + err.Error())
	}
	return d
}

// ParseJSON reads a JSON number literal (RFC 8259 §6): an optional minus, an
// integer part without leading zeros, an optional fraction, an optional
// exponent. It is how a hand-written plan's {"number": …} is read, so that
// "1e23" means exactly 10^23 rather than the float64 nearest to it.
//
// The exponent is range-checked BEFORE any digits are built: see the package
// comment's BOUNDS.
func ParseJSON(lit string) (Decimal, error) {
	s := lit
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	// Split off the exponent.
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e := s[i+1:]
		s = s[:i]
		sign := 1
		switch {
		case strings.HasPrefix(e, "+"):
			e = e[1:]
		case strings.HasPrefix(e, "-"):
			sign, e = -1, e[1:]
		}
		if e == "" || !allDigits(e) {
			return Decimal{}, ErrSyntax
		}
		// Any exponent this long is out of range whatever the mantissa; the
		// check also keeps Atoi from overflowing.
		if len(strings.TrimLeft(e, "0")) > 7 {
			return Decimal{}, ErrOutOfRange
		}
		n, err := strconv.Atoi(e)
		if err != nil {
			return Decimal{}, ErrSyntax
		}
		exp = sign * n
	}
	intPart, fracPart, hasPoint := strings.Cut(s, ".")
	if intPart == "" || !allDigits(intPart) || (hasPoint && (fracPart == "" || !allDigits(fracPart))) {
		return Decimal{}, ErrSyntax
	}
	// JSON forbids leading zeros ("01"); accepting them would read text the
	// JSON grammar says is not a number.
	if len(intPart) > 1 && intPart[0] == '0' {
		return Decimal{}, ErrSyntax
	}
	d, err := build(neg, intPart, fracPart)
	if err != nil {
		return Decimal{}, err
	}
	return d.Shift(exp)
}

// FromInt64 is the exact Decimal of n.
func FromInt64(n int64) Decimal {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	d, _ := build(neg, strings.TrimPrefix(s, "-"), "") // an int64 is always in range
	return d
}

// build makes a canonical Decimal from a sign and the digits either side of
// the decimal point, and applies the range check.
func build(neg bool, intPart, fracPart string) (Decimal, error) {
	intPart = strings.TrimLeft(intPart, "0")
	fracPart = strings.TrimRight(fracPart, "0")
	if len(intPart) > MaxIntegerDigits || len(fracPart) > MaxFractionDigits {
		return Decimal{}, ErrOutOfRange
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	if digits == "" {
		return Decimal{digits: "0"}, nil // zero: never negative, scale 0
	}
	return Decimal{neg: neg, digits: digits, scale: len(fracPart)}, nil
}

// parts returns the digits before and after the decimal point, without
// leading or trailing zeros: "" for an empty side.
func (d Decimal) parts() (intPart, fracPart string) {
	if d.digits == "0" {
		return "", ""
	}
	if d.scale == 0 {
		return d.digits, ""
	}
	if d.scale >= len(d.digits) {
		return "", strings.Repeat("0", d.scale-len(d.digits)) + d.digits
	}
	return d.digits[:len(d.digits)-d.scale], d.digits[len(d.digits)-d.scale:]
}

// String is the canonical text: "-12.5", "0", "99999999999999999999999". It
// never uses an exponent, so it reads the same to a person, to PostgreSQL and
// to JSON. The zero Decimal, which is not a number, renders as "?".
func (d Decimal) String() string {
	if !d.Valid() {
		return "?"
	}
	intPart, fracPart := d.parts()
	if intPart == "" {
		intPart = "0"
	}
	var b strings.Builder
	if d.neg {
		b.WriteByte('-')
	}
	b.WriteString(intPart)
	if fracPart != "" {
		b.WriteByte('.')
		b.WriteString(fracPart)
	}
	return b.String()
}

// Sign is -1, 0 or +1. The zero Decimal (not a number) is 0; callers check
// Valid first.
func (d Decimal) Sign() int {
	switch {
	case !d.Valid() || d.digits == "0":
		return 0
	case d.neg:
		return -1
	}
	return 1
}

// IsInteger reports a whole number. Canonical form makes this exact: a
// fraction never ends in zero, so scale > 0 means a non-zero fraction.
func (d Decimal) IsInteger() bool { return d.Valid() && d.scale == 0 }

// Int64 returns d as an int64 when d is a whole number in int64's range.
func (d Decimal) Int64() (int64, bool) {
	if !d.IsInteger() {
		return 0, false
	}
	s := d.digits
	if d.neg {
		s = "-" + s
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// Neg is -d.
func (d Decimal) Neg() Decimal {
	if d.Sign() == 0 {
		return d
	}
	d.neg = !d.neg
	return d
}

// Shift is d × 10^n, exactly: the decimal point moves n places (to the right
// for positive n). It is how a magnitude suffix ("50k", "1.5m") and a JSON
// exponent are applied without arithmetic that could round.
func (d Decimal) Shift(n int) (Decimal, error) {
	if !d.Valid() {
		return Decimal{}, ErrSyntax
	}
	if d.digits == "0" || n == 0 {
		return d, nil
	}
	// Bound before building: the result's integer digits are at least n
	// minus the fraction's length, and its fraction digits at least -n minus
	// the integer's length. Either past its limit is out of range whatever
	// the digits are.
	if n > MaxIntegerDigits+MaxFractionDigits || -n > MaxIntegerDigits+MaxFractionDigits {
		return Decimal{}, ErrOutOfRange
	}
	intPart, fracPart := d.parts()
	all := intPart + fracPart
	point := len(intPart) + n // the new position of the decimal point in all
	switch {
	case point <= 0:
		return build(d.neg, "", strings.Repeat("0", -point)+all)
	case point >= len(all):
		return build(d.neg, all+strings.Repeat("0", point-len(all)), "")
	}
	return build(d.neg, all[:point], all[point:])
}

// Cmp compares two valid Decimals: -1 if d < o, 0 if equal, +1 if d > o.
// Exact, by digits: no conversion to a machine number.
func (d Decimal) Cmp(o Decimal) int {
	if ds, os := d.Sign(), o.Sign(); ds != os {
		if ds < os {
			return -1
		}
		return 1
	}
	mag := cmpMagnitude(d, o)
	if d.neg {
		return -mag
	}
	return mag
}

// cmpMagnitude compares |a| and |b|.
func cmpMagnitude(a, b Decimal) int {
	ai, af := a.parts()
	bi, bf := b.parts()
	if len(ai) != len(bi) {
		if len(ai) < len(bi) {
			return -1
		}
		return 1
	}
	if c := strings.Compare(ai, bi); c != 0 {
		return c
	}
	// Equal integer parts: compare the fractions digit by digit, padding the
	// shorter with zeros (which is what a missing digit is worth).
	for len(af) < len(bf) {
		af += "0"
	}
	for len(bf) < len(af) {
		bf += "0"
	}
	return strings.Compare(af, bf)
}

// MarshalJSON writes the canonical text as a JSON number literal, so the
// digits survive the trip exactly. The zero Decimal is not a number and does
// not marshal.
func (d Decimal) MarshalJSON() ([]byte, error) {
	if !d.Valid() {
		return nil, errors.New("numeric: the zero Decimal is not a number")
	}
	return []byte(d.String()), nil
}

// UnmarshalJSON reads a JSON number literal exactly (ParseJSON). A JSON
// string, even one holding digits, is refused: {"number": "5"} leaves the
// type implicit, which the plan format does not allow.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	v, err := ParseJSON(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// GoString covers %#v, which would otherwise show the internal fields.
func (d Decimal) GoString() string { return "numeric.Decimal(" + d.String() + ")" }

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
