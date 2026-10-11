package numeric

import (
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The regression the package exists for (Phase 8 finding V02): a 23-digit
// literal must come out with every digit, where float64 gave 1e23.
func TestLargeIntegerIsExact(t *testing.T) {
	const lit = "99999999999999999999999"
	d, err := Parse(lit)
	if err != nil {
		t.Fatal(err)
	}
	if d.String() != lit {
		t.Fatalf("String = %s, want %s", d, lit)
	}
	// The float64 route this replaces really does lose it — otherwise this
	// test would prove nothing.
	if f, _ := strconv.ParseFloat(lit, 64); strconv.FormatFloat(f, 'f', -1, 64) == lit {
		t.Fatal("float64 held the literal exactly; the regression input is too small to test anything")
	}
	if _, ok := d.Int64(); ok {
		t.Error("a 23-digit integer fits no int64; Int64 must say so")
	}
	if !d.IsInteger() || d.Sign() != 1 {
		t.Errorf("IsInteger=%v Sign=%d", d.IsInteger(), d.Sign())
	}
}

// High-precision decimals: every digit kept, on both sides of the point.
func TestHighPrecisionDecimalIsExact(t *testing.T) {
	for _, lit := range []string{
		"0.1234567890123456789012345",
		"12345678901234567890.0987654321",
		"-0.000000000000000000000001",
		"3.1415926535897932384626433832795028841971693993751",
	} {
		d, err := Parse(lit)
		if err != nil {
			t.Fatalf("%s: %v", lit, err)
		}
		if d.String() != lit {
			t.Errorf("String = %s, want %s", d, lit)
		}
	}
}

func TestCanonicalForm(t *testing.T) {
	cases := map[string]string{
		"0": "0", "000": "0", "-0": "0", "0.000": "0", "-0.0": "0",
		"007": "7", "9.90": "9.9", "100": "100", "100.0": "100",
		"0.50": "0.5", "-12.500": "-12.5", "1500000": "1500000",
	}
	for in, want := range cases {
		d, err := Parse(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if d.String() != want {
			t.Errorf("Parse(%q) = %s, want %s", in, d, want)
		}
	}
	// Same value, same fields: equality by value is equality of the struct.
	if MustParse("9.90") != MustParse("9.9") || MustParse("-0") != MustParse("0") {
		t.Error("equal values produced unequal Decimals")
	}
}

// Parse is the canonical form only. Every wider notation is its caller's to
// reduce first, so none of these may be read here.
func TestParseRejectsEverythingElse(t *testing.T) {
	for _, in := range []string{
		"", "-", "+5", "5.", ".5", "1e5", "1E5", "1,000", " 5", "5 ", "5k",
		"0x10", "1_000", "NaN", "Inf", "--5", "1.2.3", "٣", "５",
	} {
		if d, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %s, want an error", in, d)
		}
	}
}

func TestParseJSON(t *testing.T) {
	cases := map[string]string{
		"5":        "5",
		"-5":       "-5",
		"1.5":      "1.5",
		"1e23":     "100000000000000000000000",
		"1E+2":     "100",
		"1.5e-3":   "0.0015",
		"-2.50e1":  "-25",
		"0":        "0",
		"-0.0e5":   "0",
		"12.34e-5": "0.0001234",
	}
	for in, want := range cases {
		d, err := ParseJSON(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if d.String() != want {
			t.Errorf("ParseJSON(%q) = %s, want %s", in, d, want)
		}
	}
	for _, in := range []string{"01", "+1", ".5", "5.", "1e", "1e+", `"5"`, "true", "null", "1.2.3", "0x1p4"} {
		if d, err := ParseJSON(in); err == nil {
			t.Errorf("ParseJSON(%q) = %s, want an error", in, d)
		}
	}
}

// The bound is checked before any digits are built: an eleven-byte literal
// must not allocate a billion digits.
func TestRangeIsPostgresNumeric(t *testing.T) {
	for _, in := range []string{"1e999999999", "1e99999999999999999999", "1e-999999999", "1e131072", "1e-16384"} {
		if _, err := ParseJSON(in); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("ParseJSON(%q) err = %v, want ErrOutOfRange", in, err)
		}
	}
	// The edges themselves are in range.
	if _, err := ParseJSON("1e131071"); err != nil {
		t.Errorf("131072 integer digits: %v", err)
	}
	if _, err := ParseJSON("1e-16383"); err != nil {
		t.Errorf("16383 fraction digits: %v", err)
	}
	if _, err := Parse(strings.Repeat("9", MaxIntegerDigits+1)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("too many integer digits: %v", err)
	}
	if _, err := Parse("0." + strings.Repeat("9", MaxFractionDigits+1)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("too many fraction digits: %v", err)
	}
}

func TestShift(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"50", 3, "50000"},
		{"1.5", 6, "1500000"},
		{"1.25", 1, "12.5"},
		{"0.001", 3, "1"},
		{"4.35", 3, "4350"}, // no product: the float64 route rounds back to 4350 only by luck
		{"0.1", 3, "100"},
		{"123", -2, "1.23"},
		{"123", -5, "0.00123"},
		{"-7", 2, "-700"},
		{"0", 9, "0"},
	}
	for _, c := range cases {
		got, err := MustParse(c.in).Shift(c.n)
		if err != nil {
			t.Fatalf("%s shift %d: %v", c.in, c.n, err)
		}
		if got.String() != c.want {
			t.Errorf("%s shift %d = %s, want %s", c.in, c.n, got, c.want)
		}
	}
	if _, err := MustParse("1").Shift(math.MaxInt32); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("huge shift: %v", err)
	}
	if _, err := (Decimal{}).Shift(1); err == nil {
		t.Error("shifting the zero Decimal (not a number) must fail")
	}
}

func TestCmp(t *testing.T) {
	ordered := []string{
		"-100000000000000000000001", "-100000000000000000000000", "-1.5", "-1.25", "-1",
		"-0.001", "0", "0.001", "0.1", "0.10000000000000000001", "1", "1.25", "1.5",
		"9007199254740992", "9007199254740993", "99999999999999999999999",
	}
	for i := range ordered {
		for j := range ordered {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := MustParse(ordered[i]).Cmp(MustParse(ordered[j])); got != want {
				t.Errorf("Cmp(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestInt64(t *testing.T) {
	cases := []struct {
		in string
		n  int64
		ok bool
	}{
		{"0", 0, true},
		{"50000", 50000, true},
		{"-5", -5, true},
		{"9223372036854775807", math.MaxInt64, true},
		{"-9223372036854775808", math.MinInt64, true},
		{"9223372036854775808", 0, false},
		{"9007199254740993", 9007199254740993, true}, // float64 would give …992
		{"1.5", 0, false},
	}
	for _, c := range cases {
		n, ok := MustParse(c.in).Int64()
		if ok != c.ok || (ok && n != c.n) {
			t.Errorf("Int64(%s) = %d, %v; want %d, %v", c.in, n, ok, c.n, c.ok)
		}
	}
	if FromInt64(math.MinInt64).String() != "-9223372036854775808" || FromInt64(0).String() != "0" {
		t.Error("FromInt64 is not exact at the edges")
	}
}

// The zero Decimal is not a number, and nothing may treat it as zero.
func TestZeroValueIsNotANumber(t *testing.T) {
	var d Decimal
	if d.Valid() || d.IsInteger() || d.Sign() != 0 {
		t.Errorf("zero Decimal: Valid=%v IsInteger=%v Sign=%d", d.Valid(), d.IsInteger(), d.Sign())
	}
	if _, ok := d.Int64(); ok {
		t.Error("zero Decimal converted to an int64")
	}
	if _, err := json.Marshal(d); err == nil {
		t.Error("zero Decimal marshalled")
	}
	if d.String() != "?" {
		t.Errorf("String = %q", d.String())
	}
}

// JSON round trip keeps every digit, as a number literal (not a string).
func TestJSONRoundTrip(t *testing.T) {
	for _, lit := range []string{"99999999999999999999999", "0.1234567890123456789", "-42", "0"} {
		b, err := json.Marshal(MustParse(lit))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != lit {
			t.Errorf("Marshal = %s, want %s", b, lit)
		}
		var back Decimal
		if err := json.Unmarshal(b, &back); err != nil || back.String() != lit {
			t.Errorf("Unmarshal(%s) = %s, %v", b, back, err)
		}
	}
	var d Decimal
	if err := json.Unmarshal([]byte(`"5"`), &d); err == nil {
		t.Error("a JSON string was read as a number")
	}
}

// The range bound is checked BEFORE digits are built. Without it the result
// is still an error, but only after building every digit: a request-sized
// input must not cost gigabytes (found by mutation: removing the early bound
// passed every value test).
func TestOutOfRangeIsRefusedWithoutBuildingIt(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, n := range []int{math.MaxInt32, -math.MaxInt32, MaxIntegerDigits + MaxFractionDigits + 1} {
		if _, err := MustParse("1").Shift(n); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Shift(%d): %v", n, err)
		}
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("refusing three out-of-range shifts allocated %d bytes; the bound must come first", grew)
	}
}
