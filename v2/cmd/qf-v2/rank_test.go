package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/qferr"
)

// CLI tests for --rank and --rank-probe. Deterministic embedder throughout:
// these test plumbing, exit codes and redaction. Ranking DECISIONS are tested
// exactly, with scripted retrieval, in internal/ranking; ranking QUALITY is
// measured against the real model in docs/phase-4-results.md.

func TestRankRunsAndReportsAReading(t *testing.T) {
	code, stdout, stderr := invoke(
		"--config", fixture("ambiguity.json"),
		"--rank", "orders with amount over 100",
		"--embedder", "deterministic",
	)
	// The hashing embedder may or may not produce an accept; either exit is a
	// valid outcome, but it must be one of the two and must be coded.
	if code != exitOK && code != exitFailure {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"query:", "policy:", "reading", "verdict:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if code == exitFailure && !strings.Contains(stderr, "AMBIGUOUS_MATCH") && !strings.Contains(stderr, "NO_SEMANTIC_MATCH") {
		t.Errorf("a refusal must carry a ranking code:\n%s", stderr)
	}
}

// Section 12: the full ranking object is --debug only.
func TestRankDiagnosticsOnlyUnderDebug(t *testing.T) {
	args := []string{"--config", fixture("ambiguity.json"), "--rank", "amount of the payment", "--embedder", "deterministic"}
	_, plain, _ := invoke(args...)
	_, debug, _ := invoke(append(args, "--debug")...)
	for _, marker := range []string{"-- candidates for covered spans --", "lex  type"} {
		if strings.Contains(plain, marker) {
			t.Errorf("default output contains the debug section %q", marker)
		}
		if !strings.Contains(debug, marker) {
			t.Errorf("--debug output lacks %q", marker)
		}
	}
}

func TestRankJSONIsWellFormed(t *testing.T) {
	_, stdout, _ := invoke("--config", fixture("ambiguity.json"), "--rank", "amount",
		"--embedder", "deterministic", "--json")
	var out struct {
		Verdict string `json:"verdict"`
		Policy  struct {
			MinScore  float64 `json:"minScore"`
			MinMargin float64 `json:"minMargin"`
		} `json:"policy"`
		AllSpans []any `json:"allSpans"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if out.Policy.MinScore == 0 || out.Verdict == "" {
		t.Errorf("missing fields: %+v", out)
	}
	if out.AllSpans != nil {
		t.Error("allSpans is the full ranking object and must be --debug only")
	}
}

func TestRankRefusesAnInvalidPolicy(t *testing.T) {
	for _, args := range [][]string{
		{"--min-score", "0"},
		{"--min-score", "-1"},
		{"--min-margin", "-0.1"},
		{"--min-margin", "5"},
	} {
		code, _, stderr := invoke(append([]string{"--config", fixture("ambiguity.json"),
			"--rank", "amount", "--embedder", "deterministic"}, args...)...)
		if code != exitFailure || !strings.Contains(stderr, string(qferr.CodeInvalidInput)) {
			t.Errorf("%v: exit %d, stderr %s; want INVALID_INPUT", args, code, stderr)
		}
	}
}

// A result under hand-set thresholds must say so.
func TestOverriddenThresholdsAreAnnounced(t *testing.T) {
	_, stdout, _ := invoke("--config", fixture("ambiguity.json"), "--rank", "amount",
		"--embedder", "deterministic", "--min-margin", "0.2")
	if !strings.Contains(stdout, "OVERRIDDEN") {
		t.Errorf("an overridden threshold was not announced:\n%s", stdout)
	}
	_, stdout, _ = invoke("--config", fixture("ambiguity.json"), "--rank", "amount", "--embedder", "deterministic")
	if strings.Contains(stdout, "OVERRIDDEN") {
		t.Error("the default policy was reported as overridden")
	}
}

func TestRankProbeRejectsARowWhosePhraseIsNotInTheSentence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.tsv")
	if err := os.WriteFile(path, []byte("show orders\tpayments\tentity:payment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := invoke("--config", fixture("ambiguity.json"), "--rank-probe", path, "--embedder", "deterministic")
	if code != exitFailure || !strings.Contains(stderr, "INVALID_INPUT") {
		t.Errorf("exit %d, stderr %s; a broken row must be refused, not scored as a miss", code, stderr)
	}
}

func TestRankProbeRunsTheCommittedSets(t *testing.T) {
	for _, set := range []struct{ cfg, tsv string }{
		{"ambiguity.json", "phase4-ambiguity.tsv"},
		{"commerce.json", "phase4-commerce.tsv"},
	} {
		code, stdout, stderr := invoke("--config", fixture(set.cfg),
			"--rank-probe", filepath.Join("..", "..", "testdata", "queries", set.tsv),
			"--embedder", "deterministic")
		if code != exitOK {
			t.Fatalf("%s: exit %d: %s", set.tsv, code, stderr)
		}
		if !regexp.MustCompile(`RANKPROBE raw=\d+ threshold=\d+ ranked=\d+ scored=\d+`).MatchString(stdout) {
			t.Errorf("%s: no machine-readable summary line", set.tsv)
		}
		if !strings.Contains(stdout, "NOT SEMANTIC") {
			t.Errorf("%s: a hashing-embedder probe must be labelled NOT SEMANTIC", set.tsv)
		}
	}
}

// ---------------------------------------------------------------------------
// Redaction sweep
// ---------------------------------------------------------------------------

// fixtureValues reads every config value from a fixture via the corpus: enum
// values, alias keys, alias targets. Nothing listed by hand.
func fixtureValues(t *testing.T, name string) []string {
	t.Helper()
	cfg, err := config.Load(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range corpus.Build(config.NewSchemaRegistry(cfg)) {
		if e.Kind != corpus.KindValueAlias {
			continue
		}
		for _, v := range append(e.Texts(), e.Meta.ValueTarget) {
			if v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// leaked returns the config values in output that the user did not type.
//
// The rule is exactly the contract: a value the user typed may be echoed (as
// --analyze echoes the sentence), anything else from the data domain may not.
// Matching is whole-word and case-sensitive against the output, but the
// "did the user type it" test is case-insensitive — typing "freedonia" permits
// echoing "freedonia", and still forbids nothing else.
func leaked(output, input string, values []string) []string {
	var out []string
	lowerInput := strings.ToLower(input)
	for _, v := range values {
		if strings.Contains(lowerInput, strings.ToLower(v)) {
			continue
		}
		if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(v) + `($|[^A-Za-z0-9_])`).MatchString(output) {
			out = append(out, v)
		}
	}
	return out
}

// The sweep: every sentence, every output mode, default rendering.
//
// The MISSPELLED sentences are what give it teeth. A value the user typed may
// be echoed, so a sweep of sentences that type their values can never see a
// leak — and a mutation test showed exactly that: with DisplayID disabled
// entirely, the first version of this sweep still passed. "Freedon" retrieves
// the Freedonia alias without the user having typed "Freedonia", so any
// rendering of that ID or surface is caught.
func TestRankLeaksNoConfigValueByDefault(t *testing.T) {
	for _, cfg := range []string{"ambiguity.json", "commerce.json"} {
		values := fixtureValues(t, cfg)
		for _, sentence := range []string{
			"customers from Freedon",
			"payments that bounce",
			"customers from Sylvan",
			"customers from Indi",
			"customers from Freedonia",
			"customers from India",
			"payments that bounced",
			"customers who are zorbified",
			"status",
			"country of the customer",
			"laptops",
			"orders with amount over 100",
		} {
			for _, mode := range [][]string{nil, {"--json"}} {
				args := append([]string{"--config", fixture(cfg), "--rank", sentence,
					"--embedder", "deterministic", "--top-k", "20"}, mode...)
				_, stdout, stderr := invoke(args...)
				if l := leaked(stdout+stderr, sentence, values); len(l) > 0 {
					t.Errorf("%s %q %v: default output leaks %v", cfg, sentence, mode, l)
				}
			}
		}
	}
}

// ...and --debug does reveal them: otherwise the sweep above would pass for a
// CLI that simply never retrieved a value.
func TestRankRevealsValuesUnderDebug(t *testing.T) {
	values := fixtureValues(t, "ambiguity.json")
	_, stdout, _ := invoke("--config", fixture("ambiguity.json"), "--rank", "zorbified customers",
		"--embedder", "deterministic", "--top-k", "20", "--debug")
	if len(leaked(stdout, "", values)) == 0 {
		t.Errorf("--debug revealed no config value; the redaction sweep proves nothing:\n%s", stdout)
	}
}

// REGRESSION (Phase 2, found in Phase 4): --search printed value-entry IDs
// verbatim — "value:customer.country:alias:India" — in both text and JSON,
// while the matched column beside it said <redacted>. The Phase 2 gate check
// looked only for <redacted>, so it passed.
func TestSearchRedactsValueIDs(t *testing.T) {
	values := fixtureValues(t, "commerce.json")
	for _, mode := range [][]string{nil, {"--json"}} {
		// "country" does not contain any value, so every value in the output
		// would be a leak — including from IDs of runner-up value entries.
		_, stdout, _ := invoke(append([]string{"--config", fixture("commerce.json"), "--search", "country",
			"--embedder", "deterministic", "--top-k", "55"}, mode...)...)
		if !strings.Contains(stdout, "VALUE_ALIAS") && !strings.Contains(stdout, `"VALUE_ALIAS"`) {
			t.Fatalf("setup: no value entry was retrieved, so the test proves nothing")
		}
		if l := leaked(stdout, "country", values); len(l) > 0 {
			t.Errorf("--search %v leaks %v", mode, l)
		}
	}
	_, debug, _ := invoke("--config", fixture("commerce.json"), "--search", "country",
		"--embedder", "deterministic", "--top-k", "55", "--debug")
	if !strings.Contains(debug, "alias:India") {
		t.Error("--debug should reveal value ids")
	}
}

// The same leak, in the Phase 2 probe report.
func TestProbeRedactsValueIDs(t *testing.T) {
	_, stdout, _ := invoke("--config", fixture("commerce.json"),
		"--probe", filepath.Join("..", "..", "testdata", "queries", "phase2-probes.tsv"),
		"--embedder", "deterministic")
	for _, leak := range []string{"alias:India", "alias:laptops", "enum:suspended"} {
		if strings.Contains(stdout, leak) {
			t.Errorf("probe report contains %q", leak)
		}
	}
}
