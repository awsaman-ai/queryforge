package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// CLI tests for --compile and --plan. SQL correctness is tested exactly in
// internal/compiler and internal/plan; these cover plumbing, exit codes,
// redaction and determinism across invocations.

func planFixture(name string) string {
	return filepath.Join("..", "..", "testdata", "plans", name)
}

func TestCompilePrintsSQLAndParams(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("commerce.json"), "--compile", planFixture("brief-example.json"))
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{`FROM "customers" AS "t0"`, "LIMIT 10;", "params:", "2 = 50000 (number)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

// A string parameter is a config value (the alias target "IN"): redacted by
// default, shown under --debug (decision 006).
func TestCompileRedactsStringParamsByDefault(t *testing.T) {
	_, stdout, _ := invoke("--config", fixture("commerce.json"), "--compile", planFixture("brief-example.json"))
	if strings.Contains(stdout, `"IN"`) || !strings.Contains(stdout, "1 = <redacted> (string)") {
		t.Errorf("default output:\n%s", stdout)
	}
	_, stdout, _ = invoke("--config", fixture("commerce.json"), "--compile", planFixture("brief-example.json"), "--debug")
	if !strings.Contains(stdout, `1 = "IN" (string)`) {
		t.Errorf("--debug output:\n%s", stdout)
	}
	_, stdout, _ = invoke("--config", fixture("commerce.json"), "--compile", planFixture("brief-example.json"), "--json")
	if strings.Contains(stdout, `"IN"`) {
		t.Errorf("--json leaks without --debug:\n%s", stdout)
	}
}

func TestCompileJSON(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("commerce.json"), "--compile", planFixture("brief-example.json"), "--json", "--debug")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var out struct {
		SQL    string `json:"sql"`
		Params []struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			Value any    `json:"value"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !strings.Contains(out.SQL, `"t0"."total_spent" > $2`) || len(out.Params) != 2 {
		t.Errorf("out = %+v", out)
	}
	if out.Params[0].Value != "IN" || out.Params[1].Value != float64(50000) {
		t.Errorf("params = %+v", out.Params)
	}
}

// Section 14's gate across PROCESSES' worth of state: identical bytes.
func TestCompileIsByteIdenticalAcrossInvocations(t *testing.T) {
	for _, name := range []string{"brief-example.json", "exists-and-count.json", "group-having.json", "joins-and-subqueries.json"} {
		_, first, _ := invoke("--config", fixture("commerce.json"), "--compile", planFixture(name), "--debug")
		for i := 0; i < 5; i++ {
			if _, again, _ := invoke("--config", fixture("commerce.json"), "--compile", planFixture(name), "--debug"); again != first {
				t.Fatalf("%s differs on run %d", name, i)
			}
		}
	}
}

func TestCompileRefusalsAreCodedAndRedacted(t *testing.T) {
	for name, want := range map[string]string{
		"refuse-identifier-injection.json": "SECURITY_REJECTED",
		"refuse-unconfigured-join.json":    "SECURITY_REJECTED",
		"refuse-same-row-ambiguity.json":   "AMBIGUOUS_MATCH",
		"refuse-fanout-sum.json":           "UNSUPPORTED_QUERY",
		"refuse-unknown-key.json":          "INVALID_INPUT",
	} {
		code, stdout, stderr := invoke("--config", fixture("commerce.json"), "--compile", planFixture(name))
		if code != exitFailure || !strings.HasPrefix(stderr, want) {
			t.Errorf("%s: exit %d, stderr %q, want %s", name, code, stderr, want)
		}
		if stdout != "" {
			t.Errorf("%s: a refused plan printed output:\n%s", name, stdout)
		}
		if strings.Contains(stderr, "DROP") || strings.Contains(stderr, "customer_products") {
			t.Errorf("%s: the refusal echoes the plan's unconfigured name: %s", name, stderr)
		}
	}
}

func TestCompileMissingFile(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("commerce.json"), "--compile", planFixture("does-not-exist.json"))
	if code != exitFailure || !strings.HasPrefix(stderr, "INVALID_INPUT") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

// --compile loads no model: it must work with the worker flags pointing
// nowhere.
func TestCompileNeedsNoModel(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("commerce.json"), "--compile", planFixture("brief-example.json"),
		"--worker-script", "/nonexistent/worker.py")
	if code != exitOK {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

func TestPlanReportsAPlanOrARefusal(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("commerce.json"), "--plan", "customers from India",
		"--embedder", "deterministic", "--now", "2026-09-21T12:00:00Z")
	if code != exitOK && code != exitFailure {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "clock:    2026-09-21T12:00:00Z") || !strings.Contains(stdout, "plan") {
		t.Errorf("stdout:\n%s", stdout)
	}
	// --plan stops at the plan: no SQL, ever, before Phase 7.
	if strings.Contains(stdout, "SELECT") {
		t.Errorf("--plan printed SQL:\n%s", stdout)
	}
}

func TestPlanRejectsABadClock(t *testing.T) {
	for _, args := range [][]string{{"--now", "yesterday"}, {"--tz", "Mars/Olympus"}} {
		code, _, stderr := invoke(append([]string{"--config", fixture("commerce.json"), "--plan", "orders last month",
			"--embedder", "deterministic"}, args...)...)
		if code != exitFailure || !strings.HasPrefix(stderr, "INVALID_INPUT") {
			t.Errorf("%v: exit %d, %s", args, code, stderr)
		}
		if strings.Contains(stderr, "Mars") {
			t.Errorf("the bad timezone is echoed: %s", stderr)
		}
	}
}
