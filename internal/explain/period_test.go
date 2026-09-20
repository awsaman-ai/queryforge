package explain_test

import (
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/testutil"

	. "github.com/awsaman-ai/queryforge/internal/ast"
	. "github.com/awsaman-ai/queryforge/internal/explain"
)

// TestDescribePeriod pins the readback for every period shape the prompt
// teaches, plus the shapes with no everyday name, which must still be exact.
func TestDescribePeriod(t *testing.T) {
	cases := []struct {
		unit     string
		from, to int
		want     string
	}{
		{"day", 0, 0, "today"},
		{"day", -1, -1, "yesterday"},
		{"day", 1, 1, "tomorrow"},
		{"week", 0, 0, "this week"},
		{"week", -1, -1, "last week"},
		{"month", -1, -1, "last month"},
		{"year", 0, 0, "this year"},
		{"year", 1, 1, "next year"},
		{"day", -29, 0, "the last 30 days including today"},
		{"month", -2, 0, "the last 3 months including this month"},
		{"day", -30, -1, "the 30 days before today"},
		{"week", -1, 0, "the last 2 weeks including this week"},
		{"day", -3, -3, "the day 3 days ago"},
		{"month", -6, -4, "the month 6 months ago through the month 4 months ago"},
		{"day", 1, 7, "tomorrow through the day 7 days from now"},
		{"days", -1, -1, `<invalid unit "days">`}, // loud, never plausible
	}
	for _, tc := range cases {
		if got := DescribePeriod(tc.unit, tc.from, tc.to); got != tc.want {
			t.Errorf("DescribePeriod(%s, %d, %d) = %q, want %q", tc.unit, tc.from, tc.to, got, tc.want)
		}
	}
}

// TestExplainPeriodComparison checks the full sentence: its own verb rather
// than "is between", and the zone named only when the config set one.
func TestExplainPeriodComparison(t *testing.T) {
	cond := testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -1))

	utc := testutil.GenConfig(t)
	if got := DescribeComparison(cond, utc); got != "createdAt falls in yesterday" {
		t.Errorf("UTC config: %q", got)
	}

	ist := testutil.MustParse(t, strings.Replace(testutil.GenConfigJSON, `"entity":"Order",`, `"entity":"Order","timezone":"Asia/Kolkata",`, 1))
	if got := DescribeComparison(cond, ist); got != "createdAt falls in yesterday (Asia/Kolkata time)" {
		t.Errorf("IST config: %q", got)
	}
}
