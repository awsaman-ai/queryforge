package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
)

func fixture(name string) string {
	return filepath.Join("..", "..", "testdata", "configs", name)
}

// invoke runs the CLI in-process and returns its exit code and streams.
func invoke(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestValidateConfigSucceeds(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("customers.json"), "--validate-config")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "config OK") {
		t.Errorf("stdout = %q, want a config OK line", stdout)
	}
}

func TestValidateConfigJSON(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("customers.json"), "--validate-config", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	var got struct {
		OK       bool `json:"ok"`
		Entities int  `json:"entities"`
		Fields   int  `json:"fields"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if !got.OK || got.Entities != 1 || got.Fields != 4 {
		t.Errorf("got %+v, want ok with 1 entity and 4 fields", got)
	}
}

// TestQueryIsDerivedOrRefused REPLACES Phase 0's TestQueryIsNotImplemented,
// as that test asked. Its promise was that the CLI never emits a query it did
// not derive. With the pipeline built, that becomes: SQL reaches stdout only on
// success (exit 0), a refusal prints no SQL and carries a stable code, and
// NOT_IMPLEMENTED can no longer occur. What the pipeline DERIVES is asserted
// exactly in internal/pipeline (TestBriefExamplesEndToEnd); this test uses the
// deterministic embedder, so it checks the contract, not the semantics.
func TestQueryIsDerivedOrRefused(t *testing.T) {
	for _, q := range []string{"show customers from India", "customers from Atlantis", "zzz qqq", "customers who bought laptops"} {
		code, stdout, stderr := invoke("--config", fixture("commerce.json"), "--query", q,
			"--embedder", "deterministic", "--now", "2026-09-21T12:00:00Z")
		if strings.Contains(stderr, string(qferr.CodeNotImplemented)) {
			t.Fatalf("%q: the pipeline still reports NOT_IMPLEMENTED", q)
		}
		hasSQL := strings.Contains(stdout, "SELECT ")
		switch code {
		case exitOK:
			if !hasSQL || !strings.Contains(stdout, "FROM ") {
				t.Errorf("%q: success without a query: %q", q, stdout)
			}
		case exitFailure:
			if hasSQL {
				t.Errorf("%q: a refusal printed SQL: %q", q, stdout)
			}
			if qferr.Code(strings.SplitN(stderr, ":", 2)[0]) == "" {
				t.Errorf("%q: refusal without a code: %q", q, stderr)
			}
		default:
			t.Errorf("%q: exit %d: %s", q, code, stderr)
		}
	}
}

func TestUnsupportedBackendIsRejected(t *testing.T) {
	for _, backend := range []string{"mysql", "mongodb", "elasticsearch", "opensearch", ""} {
		t.Run(backend, func(t *testing.T) {
			code, _, stderr := invoke(
				"--config", fixture("customers.json"),
				"--query", "show customers",
				"--backend", backend,
			)
			if code != exitFailure {
				t.Fatalf("exit = %d, want %d", code, exitFailure)
			}
			if !strings.Contains(stderr, "INVALID_INPUT") {
				t.Errorf("stderr = %q, want INVALID_INPUT", stderr)
			}
		})
	}
}

func TestBadConfigReportsConfigError(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("invalid-no-entities.json"), "--query", "show customers")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "CONFIG_ERROR") {
		t.Errorf("stderr = %q, want CONFIG_ERROR", stderr)
	}
}

func TestMissingConfigFlag(t *testing.T) {
	code, _, stderr := invoke("--query", "show customers")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "--config is required") {
		t.Errorf("stderr = %q, want a --config is required message", stderr)
	}
}

// With four modes there is no single required flag, so an invocation that names
// none of them must say what the options are rather than name one arbitrarily.
func TestNoModeSelected(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("customers.json"))
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "nothing to do") {
		t.Errorf("stderr = %q, want a message listing the modes", stderr)
	}
	for _, mode := range []string{"--query", "--search", "--validate-config", "--build-index"} {
		if !strings.Contains(stderr, mode) {
			t.Errorf("stderr should mention %s: %q", mode, stderr)
		}
	}
}

// A stray positional argument usually means an unquoted query. Exit 2 separates
// that from an engine refusal so a scripted gate does not record it as a result.
func TestStrayArgumentIsUsageError(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("customers.json"), "show", "customers")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--query") {
		t.Errorf("stderr = %q, should point at --query", stderr)
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	code, _, _ := invoke("--config", fixture("customers.json"), "--nonsense")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}

func TestErrorJSONCarriesCodeAndMessage(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("invalid-no-entities.json"), "--validate-config", "--json")
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &got); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\n%s", err, stderr)
	}
	if got.Error.Code != "CONFIG_ERROR" || got.Error.Message == "" {
		t.Errorf("got %+v, want a CONFIG_ERROR with a message", got)
	}
}

// The cause behind a coded error can quote file content, so it appears only
// under --debug.
func TestCauseOnlyUnderDebug(t *testing.T) {
	_, _, quiet := invoke("--config", fixture("invalid-malformed-json.json"), "--validate-config")
	if strings.Contains(quiet, "debug:") {
		t.Errorf("cause leaked without --debug: %q", quiet)
	}
	_, _, loud := invoke("--config", fixture("invalid-malformed-json.json"), "--validate-config", "--debug")
	if !strings.Contains(loud, "debug:") {
		t.Errorf("--debug did not print the cause: %q", loud)
	}
}

func TestVersion(t *testing.T) {
	code, stdout, _ := invoke("--version")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "phase-7") {
		t.Errorf("stdout = %q, want the phase in the version string", stdout)
	}
}

// TestValidateMultiEntityConfig exercises the richer fixture through the CLI,
// which is also the only place the schema registry is built end to end.
func TestValidateMultiEntityConfig(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("commerce.json"), "--validate-config", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	var got struct {
		Entities      int `json:"entities"`
		Fields        int `json:"fields"`
		Operators     int `json:"operators"`
		Aggregations  int `json:"aggregations"`
		Relationships int `json:"relationships"`
		QueryPatterns int `json:"queryPatterns"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	want := struct{ e, f, o, a, r, p int }{3, 15, 9, 5, 2, 4}
	if got.Entities != want.e || got.Fields != want.f || got.Operators != want.o ||
		got.Aggregations != want.a || got.Relationships != want.r || got.QueryPatterns != want.p {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A config with several mistakes must report all of them in one run. The CLI is
// where that actually pays off, since the alternative is one edit-run cycle per
// problem.
func TestConfigErrorListsEveryProblem(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("invalid-relationship.json"), "--validate-config")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "CONFIG_ERROR") {
		t.Errorf("stderr = %q, want CONFIG_ERROR", stderr)
	}
	if got := strings.Count(stderr, "\n  - "); got != 3 {
		t.Errorf("listed %d problems, want 3:\n%s", got, stderr)
	}
	// Each violation must name its rule, which is what makes "rejected for the
	// expected reason" checkable rather than a matter of reading prose.
	for _, rule := range []string{"unknown_relationship_target", "invalid_cardinality"} {
		if !strings.Contains(stderr, rule) {
			t.Errorf("stderr should name rule %s:\n%s", rule, stderr)
		}
	}
}

// The redaction contract at the CLI boundary. Config VALUES never appear in
// default output; structure always does, because a message that will not say
// which field is broken is useless to whoever has to fix it.
//
// See internal/qferr/sensitive.go for the contract itself.
func TestConfigValuesRedactedByDefault(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("invalid-value-alias.json"), "--validate-config")

	for _, value := range []string{"India", "india", "Nowhere", "disabled", "suspended"} {
		if strings.Contains(stderr, value) {
			t.Errorf("default output leaks the config value %q:\n%s", value, stderr)
		}
	}
	if !strings.Contains(stderr, qferr.Placeholder) {
		t.Errorf("withheld values should be visibly redacted:\n%s", stderr)
	}
	// Structure must survive: without it the message cannot be acted on.
	for _, structural := range []string{"customer.country", "customer.status", "malformed_value_alias"} {
		if !strings.Contains(stderr, structural) {
			t.Errorf("default output should keep the structural detail %q:\n%s", structural, stderr)
		}
	}
	if !strings.Contains(stderr, "--debug") {
		t.Errorf("the message should say how to see the withheld values:\n%s", stderr)
	}
}

func TestConfigValuesRevealedUnderDebug(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("invalid-value-alias.json"), "--validate-config", "--debug")

	if !strings.Contains(stderr, "debug: values revealed:") {
		t.Fatalf("--debug should print the revealed rendering:\n%s", stderr)
	}
	for _, value := range []string{"India", "india", "Nowhere", "disabled", "suspended"} {
		if !strings.Contains(stderr, value) {
			t.Errorf("--debug should show the config value %q:\n%s", value, stderr)
		}
	}

	// The redacted rendering is still printed first, so the two can be compared
	// and so the default half of the contract is visible even in debug output.
	before, after, found := strings.Cut(stderr, "debug: values revealed:")
	if !found {
		t.Fatal("expected both renderings")
	}
	if !strings.Contains(before, qferr.Placeholder) {
		t.Errorf("the default rendering should still be redacted:\n%s", before)
	}
	if strings.Contains(after, qferr.Placeholder) {
		t.Errorf("the revealed rendering should not redact:\n%s", after)
	}
}

// JSON output has the same contract. It is the format most likely to be shipped
// somewhere it will be indexed, so a leak here travels furthest.
func TestConfigValuesRedactedInJSON(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("invalid-value-alias.json"), "--validate-config", "--json")

	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
			Cause   string `json:"cause"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &out); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\n%s", err, stderr)
	}
	for _, value := range []string{"India", "Nowhere", "disabled", "suspended"} {
		if strings.Contains(stderr, value) {
			t.Errorf("JSON output leaks the config value %q:\n%s", value, stderr)
		}
	}
	if out.Error.Detail != "" || out.Error.Cause != "" {
		t.Errorf("debug fields should be absent without --debug: %+v", out.Error)
	}
	if out.Error.Code != "CONFIG_ERROR" {
		t.Errorf("code = %q, want CONFIG_ERROR", out.Error.Code)
	}
}

func TestConfigValuesRevealedInDebugJSON(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("invalid-value-alias.json"), "--validate-config", "--json", "--debug")

	var out struct {
		Error struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &out); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\n%s", err, stderr)
	}
	if !strings.Contains(out.Error.Message, qferr.Placeholder) {
		t.Errorf("the message field stays redacted even in debug JSON: %q", out.Error.Message)
	}
	for _, value := range []string{"India", "disabled", "suspended"} {
		if !strings.Contains(out.Error.Detail, value) {
			t.Errorf("debug JSON detail should show %q: %q", value, out.Error.Detail)
		}
	}
}

// A failure with nothing to reveal must not grow an empty debug section, or the
// section stops meaning anything.
func TestNoDebugSectionWhenNothingWithheld(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("invalid-relationship.json"), "--validate-config", "--debug")
	if strings.Contains(stderr, "debug: values revealed:") {
		t.Errorf("this config has no withheld values, so there is nothing to reveal:\n%s", stderr)
	}
	if strings.Contains(stderr, "withheld") {
		t.Errorf("no redaction notice should appear:\n%s", stderr)
	}
}
