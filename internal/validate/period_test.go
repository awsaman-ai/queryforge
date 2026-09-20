package validate_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/datetime"
	"github.com/awsaman-ai/queryforge/internal/testutil"

	. "github.com/awsaman-ai/queryforge/internal/ast"
	. "github.com/awsaman-ai/queryforge/internal/validate"
)

// TestPeriodValid is the happy path: every everyday period the prompt teaches
// validates on a date field that allows between, alone and inside AND/OR/NOT.
func TestPeriodValid(t *testing.T) {
	c := validatorConfig(t)
	for name, v := range map[string]*Value{
		"today":        testutil.VPeriod("day", 0, 0),
		"yesterday":    testutil.VPeriod("day", -1, -1),
		"last 30 days": testutil.VPeriod("day", -29, 0),
		"this week":    testutil.VPeriod("week", 0, 0),
		"last month":   testutil.VPeriod("month", -1, -1),
		"this year":    testutil.VPeriod("year", 0, 0),
		"next hour":    testutil.VPeriod("hour", 1, 1),
		"at the bound": testutil.VPeriod("minute", -datetime.MaxRelativeAmount, datetime.MaxRelativeAmount),
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(testutil.Single(testutil.Comp("createdAt", OpBetween, v)), c); err != nil {
				t.Errorf("valid period rejected: %v", err)
			}
		})
	}

	q := NewQuery("Order")
	q.Filter = testutil.Or(
		testutil.Not(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("month", -1, -1))),
		testutil.And(
			testutil.Comp("status", OpEquals, testutil.VEnum("PLACED")),
			testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", 0, 0)),
		),
	)
	if err := Validate(q, c); err != nil {
		t.Errorf("nested periods rejected: %v", err)
	}
}

// TestPeriodRejected is the adversarial table. Each case would compile to a
// syntactically valid query that selects the wrong rows (or none), so each must
// be a validation error the repair loop can feed back — with a message that
// names the fix.
func TestPeriodRejected(t *testing.T) {
	c := validatorConfig(t)
	cases := []struct {
		name     string
		cond     *Condition
		wantCode ErrCode
		wantMsg  string
	}{
		{"reversed bounds", testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -30)),
			CodeInvalidRelDate, "from (-1) is after to (-30)"},
		{"plural unit", testutil.Comp("createdAt", OpBetween, testutil.VPeriod("days", -1, -1)),
			CodeInvalidRelDate, `unit "days"`},
		{"empty unit", testutil.Comp("createdAt", OpBetween, testutil.VPeriod("", -1, -1)),
			CodeInvalidRelDate, `unit ""`},
		{"quarter is not a unit", testutil.Comp("createdAt", OpBetween, testutil.VPeriod("quarter", -1, -1)),
			CodeInvalidRelDate, `unit "quarter"`},
		{"from out of range", testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -datetime.MaxRelativeAmount-1, 0)),
			CodeInvalidRelDate, "out of range"},
		{"to out of range", testutil.Comp("createdAt", OpBetween, testutil.VPeriod("year", 0, 1<<40)),
			CodeInvalidRelDate, "out of range"},
		{"with after", testutil.Comp("createdAt", OpAfter, testutil.VPeriod("month", -1, -1)),
			CodeKindMismatch, `only valid with operator "between"`},
		{"with before", testutil.Comp("createdAt", OpBefore, testutil.VPeriod("day", 0, 0)),
			CodeKindMismatch, `only valid with operator "between"`},
		{"on a number field", testutil.Comp("amount", OpBetween, testutil.VPeriod("day", -1, -1)),
			CodeKindMismatch, "period"},
		{"on an operator the field forbids", testutil.Comp("createdAt", OpEquals, testutil.VPeriod("day", 0, 0)),
			CodeOperatorNotAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(testutil.Single(tc.cond), c)
			var ves ValidationErrors
			if !errors.As(err, &ves) {
				t.Fatalf("want ValidationErrors, got %v", err)
			}
			found := false
			for _, ve := range ves {
				if ve.Code == tc.wantCode && strings.Contains(ve.Message, tc.wantMsg) {
					found = true
				}
			}
			if !found {
				t.Errorf("want code %s containing %q, got %v", tc.wantCode, tc.wantMsg, err)
			}
		})
	}
}

// TestPeriodOnFieldWithoutBetween checks a date field whose whitelist omits
// between: the operator rule fires, so a period cannot sneak in through it.
func TestPeriodOnFieldWithoutBetween(t *testing.T) {
	c := testutil.MustParse(t, `{"entity":"Order","model":{},"fields":[
	  {"name":"createdAt","type":"date","operators":["before","after"]}]}`)
	err := Validate(testutil.Single(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -1))), c)
	var ves ValidationErrors
	if !errors.As(err, &ves) || len(ves) == 0 || ves[0].Code != CodeOperatorNotAllowed {
		t.Errorf("want operator_not_allowed, got %v", err)
	}
}
