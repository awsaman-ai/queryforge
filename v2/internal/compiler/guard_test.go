package compiler

import (
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

// The output guard (Phase 8, decision 021). Every reviewed golden passes it
// (TestGoldenPlans and the query set run through Compile, which calls it);
// here, a real compiled query is mutated one way at a time, and each mutation
// must be refused — without the refusal quoting what was injected.

// guardBase is a real compiled query using every lexical category the grammar
// has: LEFT JOIN, IN (...), LIKE ... ESCAPE '\', a date range, ORDER BY with
// NULLS LAST, and LIMIT.
func guardBase(t *testing.T) (Query, func(sql string, params int) error) {
	t.Helper()
	p, err := loadPlan(t, "joins-and-subqueries")
	if err != nil {
		t.Fatal(err)
	}
	reg, g := registry(t, "commerce.json")
	q, err := Compile(p, reg, g)
	if err != nil {
		t.Fatal(err)
	}
	return q, func(sql string, params int) error { return guard(sql, params, reg) }
}

// Compile runs the guard on every query and returns its refusal, not the query.
func TestCompileCallsTheGuard(t *testing.T) {
	p, err := loadPlan(t, "brief-example")
	if err != nil {
		t.Fatal(err)
	}
	reg, g := registry(t, "commerce.json")
	calls := 0
	defer func(saved func(string, int, *config.SchemaRegistry) error) { outputGuard = saved }(outputGuard)
	outputGuard = func(string, int, *config.SchemaRegistry) error {
		calls++
		return guardFail(0, "forced by the test")
	}
	q, err := Compile(p, reg, g)
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "forced by the test") || q.SQL != "" {
		t.Fatalf("calls %d, err %v, SQL %q", calls, err, q.SQL)
	}
}

func TestGuardAcceptsTheCompilersOutput(t *testing.T) {
	q, check := guardBase(t)
	if err := check(q.SQL, len(q.Params)); err != nil {
		t.Fatalf("%v\n%s", err, q.SQL)
	}
	for _, want := range []string{"LEFT JOIN", " IN (", ` ESCAPE '\'`, "NULLS LAST", "LIMIT 25"} {
		if !strings.Contains(q.SQL, want) {
			t.Fatalf("premise: the base query should contain %q:\n%s", want, q.SQL)
		}
	}
}

func TestGuardRefusesTextOutsideTheLexicon(t *testing.T) {
	q, check := guardBase(t)
	body := strings.TrimSuffix(q.SQL, ";")
	n := len(q.Params)
	for _, c := range []struct {
		name, sql string
		params    int
		secret    string // injected text that must not appear in the message
		reason    string // the guard's own reason: refused for THIS, not by accident
	}{
		{"a second statement", body + "; DROP TABLE customers;", n, "customers", "a second statement"},
		{"no terminator", body, n, "", "does not end with the one statement terminator"},
		{"a line comment", body + " -- hidden;", n, "hidden", "a comment"},
		{"a block comment", body + " /* hidden */;", n, "hidden", "a comment"},
		{"a string literal", strings.Replace(q.SQL, "$1", "'laptop'", 1), n - 1, "laptop", "a string literal"},
		{"an unconfigured identifier", strings.Replace(q.SQL, `"t0"."amount"`, `"t0"."password"`, 1), n, "password", "not a configured table or column"},
		{"an alias-looking identifier", strings.Replace(q.SQL, `"t1"`, `"t1x"`, 1), n, "t1x", "not a configured table or column"},
		{"a keyword outside the grammar", body + " UNION SELECT 1;", n, "UNION", "not one of the grammar's keywords"},
		{"a lowercase keyword", "select" + strings.TrimPrefix(q.SQL, "SELECT"), n, "", "does not start with SELECT"},
		// $1 and $2 swapped: both are valid numbers, so only the ORDER is
		// wrong. (The first draft turned $1 into $X, which failed as not a
		// number and never reached the order check: found by mutation.)
		{"placeholders out of order", strings.NewReplacer("$1", "$2", "$2", "$1").Replace(q.SQL), n, "", "a placeholder out of order"},
		{"a placeholder that is not a number", strings.Replace(q.SQL, "$1", "$X", 1), n, "", "a placeholder out of order"},
		{"a placeholder count mismatch", q.SQL, n + 1, "", "do not match the parameters"},
		{"the ESCAPE literal elsewhere", strings.Replace(q.SQL, "$1", `'\'`, 1), n - 1, "", "a string literal"},
		{"a backtick", strings.Replace(q.SQL, `"t0"`, "`t0`", 1), n, "", "outside the grammar's lexicon"},
		{"a non-ASCII byte", strings.Replace(q.SQL, "SELECT ", "SELECT \u00a0", 1), n, "", "outside the grammar's lexicon"},
		{"a semicolon inside", strings.Replace(q.SQL, "\nFROM", ";\nFROM", 1), n, "", "a second statement"},
	} {
		// A mutation that changed nothing would pass vacuously (the first draft
		// of "a semicolon inside" replaced " FROM", which the SQL does not have).
		if c.sql == q.SQL && c.params == n {
			t.Fatalf("%s: the mutation did not change the query", c.name)
		}
		err := check(c.sql, c.params)
		if err == nil {
			t.Errorf("%s: accepted:\n%s", c.name, c.sql)
			continue
		}
		if qferr.CodeOf(err) != qferr.CodeSecurityRejected {
			t.Errorf("%s: code %s", c.name, qferr.CodeOf(err))
		}
		// The reason is pinned so a case refused by the wrong branch fails:
		// the first mutation run found two cases (literal, comment) that
		// stayed refused only by falling through to "outside the lexicon".
		if !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: refused for another reason: %v", c.name, err)
		}
		if c.secret != "" && strings.Contains(err.Error(), c.secret) {
			t.Errorf("%s: the refusal quotes the injected text: %v", c.name, err)
		}
	}
}
