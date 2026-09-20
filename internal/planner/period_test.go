package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/failure"
	"github.com/awsaman-ai/queryforge/internal/testutil"

	. "github.com/awsaman-ai/queryforge/internal/ast"
	. "github.com/awsaman-ai/queryforge/internal/planner"
	. "github.com/awsaman-ai/queryforge/internal/provider"
)

// TestPromptTodayUsesConfigTimezone: at 20:00Z it is already the next day in
// Asia/Kolkata. Rendering the date in UTC would tell the model it is still
// yesterday, and every "today" it resolves would be a day behind.
func TestPromptTodayUsesConfigTimezone(t *testing.T) {
	now := time.Date(2026, 9, 15, 20, 0, 0, 0, time.UTC)

	utc := New(plannerTestConfig(t), &StubProvider{}).SystemPrompt(now)
	if !strings.Contains(utc, "Today (UTC): 2026-09-15\n") {
		t.Errorf("UTC config must keep the original line:\n%s", utc)
	}

	istCfg := testutil.MustParse(t, strings.Replace(testutil.GenConfigJSON, `"entity":"Order",`, `"entity":"Order","timezone":"Asia/Kolkata",`, 1))
	ist := New(istCfg, &StubProvider{}).SystemPrompt(now)
	if !strings.Contains(ist, "Today (Asia/Kolkata): 2026-09-16\n") {
		t.Errorf("IST config must show the local date:\n%s", ist)
	}
}

// TestPromptDateGuidanceIsGated checks the three variants: full period guidance
// when a date field allows between, relative_date only when none does, and no
// date block at all without a queryable date field — an unused feature must
// add nothing to a config's prompt.
func TestPromptDateGuidanceIsGated(t *testing.T) {
	now := testutil.FixedNow

	full := New(plannerTestConfig(t), &StubProvider{}).SystemPrompt(now)
	for _, want := range []string{
		"Dates:\n",
		`{"kind":"period","unit":"day","from":-1,"to":-1}`,
		"last 30 days: day -29..0",
		"last 7 days: day -6..0 (includes today)",
		"last week: week -1..-1",
		"weeks start Monday",
		`{"kind":"relative_date","unit":"hour","amount":-2}`,
		"Value kinds: string, number, boolean, enum, array, date, relative_date, period.",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("prompt for a config with between on a date field is missing %q", want)
		}
	}

	noBetween := testutil.MustParse(t, `{"entity":"Order","model":{},"fields":[
	  {"name":"createdAt","type":"date","operators":["before","after"]}]}`)
	p := New(noBetween, &StubProvider{}).SystemPrompt(now)
	if strings.Contains(p, "period value") {
		t.Errorf("period guidance offered although no date field allows between:\n%s", p)
	}
	if !strings.Contains(p, `"kind":"relative_date"`) || !strings.Contains(p, "Dates:\n") {
		t.Errorf("relative_date guidance missing for a date field without between:\n%s", p)
	}

	hiddenDate := testutil.MustParse(t, `{"entity":"Order","model":{},"fields":[
	  {"name":"status","type":"string"},
	  {"name":"createdAt","type":"date","queryable":false}]}`)
	if p := New(hiddenDate, &StubProvider{}).SystemPrompt(now); strings.Contains(p, "Dates:") {
		t.Errorf("date block present although no queryable date field exists:\n%s", p)
	}
}

// TestShapeExampleTeachesPeriodForLast30Days keeps the example and the rules
// text in agreement. The example is copied closely; if it said -30..0 while the
// guidance said -29..0, the model would get "last 30 days" wrong by one day.
func TestShapeExampleTeachesPeriodForLast30Days(t *testing.T) {
	if !strings.Contains(ASTShapeExample, `"operator":"between","value":{"kind":"period","unit":"day","from":-29,"to":0}`) {
		t.Errorf("shape example no longer shows last-30-days as a period -29..0:\n%s", ASTShapeExample)
	}
	if strings.Contains(ASTShapeExample, `"relative_date"`) {
		t.Errorf("shape example still teaches a rolling relative_date for its date predicate:\n%s", ASTShapeExample)
	}
}

// TestPlanPeriodMissingBoundIsRepairable: a reply with a half-written period
// must come back as model-output failure (so the engine spends a repair on
// it), not as a transport error and not as a silently defaulted range.
func TestPlanPeriodMissingBoundIsRepairable(t *testing.T) {
	reply := `{"filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"day","from":-1}}}`
	pl := New(plannerTestConfig(t), &StubProvider{Response: reply})
	_, _, err := pl.Plan(context.Background(), "orders from yesterday", RepairHint{})
	if err == nil {
		t.Fatal("a period without \"to\" was accepted")
	}
	if !errors.Is(err, failure.ErrModelOutput) || errors.Is(err, failure.ErrModelTransport) {
		t.Errorf("want ErrModelOutput (repairable), got %v", err)
	}
	if !strings.Contains(err.Error(), `"from" and "to"`) {
		t.Errorf("the repair hint must name the fix, got %v", err)
	}
}

// TestPlanPeriodHappyPath decodes a well-formed period reply end to end.
func TestPlanPeriodHappyPath(t *testing.T) {
	reply := `{"filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"month","from":-1,"to":-1}}}`
	pl := New(plannerTestConfig(t), &StubProvider{Response: reply})
	q, _, err := pl.Plan(context.Background(), "orders from last month", RepairHint{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if v := q.Filter.Value; v.Kind != KindPeriod || v.Unit != "month" || v.From != -1 || v.To != -1 {
		t.Errorf("decoded %+v", v)
	}
}
