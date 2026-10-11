package inputguard

import (
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
)

// u is one character by code point, so the tests name invisible and
// look-alike characters by number rather than hiding them in the source.
func u(cp rune) string { return string(cp) }

func TestFindsEveryDelimiter(t *testing.T) {
	cases := []struct {
		in   string
		want Delimiter
		at   int
	}{
		// ASCII.
		{"customers from India; select * from orders", StatementSeparator, 20},
		{"customers from India;", StatementSeparator, 20},
		{";", StatementSeparator, 0},
		{"customers named Smith --", LineComment, 22},
		{"' OR 1=1 --", LineComment, 9},
		{"customers/**/from/**/India", BlockCommentOpen, 9},
		{"customers */ from India", BlockCommentClose, 10},
		// The first one wins, by position.
		{"a -- b; c", LineComment, 2},
		// Normalize's view: an invisible character cannot split a marker,
		// and dashes it folds to "-" are read as "-".
		{"customers -" + u(0x200B) + "- India", LineComment, 10},
		{"customers " + u(0x2014) + u(0x2014) + " India", LineComment, 10},
		{"customers " + u(0x2212) + u(0x2212) + " India", LineComment, 10},
		{"customers /" + u(0x00AD) + "* x", BlockCommentOpen, 10},
		// Compatibility forms (NFKC), including the one NFC maps to ";".
		{"India" + u(0x037E) + " orders", StatementSeparator, 5},
		{"India" + u(0xFE14) + " orders", StatementSeparator, 5},
		{"India" + u(0xFE54) + " orders", StatementSeparator, 5},
		{"India" + u(0xFF1B) + " orders", StatementSeparator, 5},
		{"India " + u(0xFE63) + u(0xFF0D) + " orders", LineComment, 6},
		{"India " + u(0xFF0F) + u(0xFF0A) + " orders", BlockCommentOpen, 6},
		{"India " + u(0xFE61) + "/ orders", BlockCommentClose, 6},
	}
	for _, c := range cases {
		d, at, ok := Find(c.in)
		if !ok || d != c.want || at != c.at {
			t.Errorf("Find(%q) = %q at %d (%v); want %q at %d", c.in, d, at, ok, c.want, c.at)
		}
	}
}

// What is NOT a delimiter stays readable: a hyphen, a slash, a star, a hash,
// spaced forms, and ordinary punctuation.
func TestLeavesOrdinaryPunctuationAlone(t *testing.T) {
	for _, in := range []string{
		"customers from India",
		"e-mail of customers", "top-5 customers", "customers - India",
		"orders/customers", "5 * 3", "order #12", "- -", "/ *", "* /",
		"customers, orders: amount? (India)!", "O'Brien", `"quoted"`,
		"customers " + u(0x2014) + " India", // one dash
	} {
		if d, at, ok := Find(in); ok {
			t.Errorf("Find(%q) = %q at %d; want none", in, d, at)
		}
	}
}

// The table is Unicode 16.0's NFKC preimage of ; - / * — pinned whole, so an
// edit to it is a visible change to a security rule.
func TestCompatTableIsPinned(t *testing.T) {
	want := map[rune]rune{
		0x037E: ';', 0xFE14: ';', 0xFE54: ';', 0xFF1B: ';',
		0xFE63: '-', 0xFF0D: '-',
		0xFF0F: '/',
		0xFE61: '*', 0xFF0A: '*',
	}
	if len(compat) != len(want) {
		t.Fatalf("compat has %d entries, want %d", len(compat), len(want))
	}
	for r, to := range want {
		if compat[r] != to {
			t.Errorf("compat[U+%04X] = %q, want %q", r, compat[r], to)
		}
	}
}

// The refusal is SECURITY_REJECTED, names the delimiter from the closed list
// and a position, and never quotes the request (decision 013).
func TestCheckRefusesWithoutQuotingTheRequest(t *testing.T) {
	err := Check("customers named Zorblatt; select * from orders")
	if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, string(StatementSeparator)) || !strings.Contains(msg, "character 24") {
		t.Errorf("message does not name the delimiter and position: %s", msg)
	}
	for _, leak := range []string{"Zorblatt", "orders", "select"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message quotes the request (%q): %s", leak, msg)
		}
	}
	if err := Check("customers from India"); err != nil {
		t.Errorf("a plain request was refused: %v", err)
	}
}
