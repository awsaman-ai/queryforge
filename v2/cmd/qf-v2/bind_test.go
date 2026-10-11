package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// CLI tests for --bind. Deterministic embedder: plumbing, exit codes and
// redaction. Binding DECISIONS are tested exactly in internal/binding.

func TestBindReportsABindingOrARefusal(t *testing.T) {
	code, stdout, stderr := invoke("--config", fixture("commerce.json"),
		"--bind", "customers from India", "--embedder", "deterministic")
	if code != exitOK && code != exitFailure {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "binding:") {
		t.Errorf("no binding section:\n%s", stdout)
	}
	if code == exitFailure && !strings.Contains(stdout, "reading:") {
		t.Error("a refusal must show the reading it refused, so it can be argued with")
	}
}

func TestBindJSONIsWellFormed(t *testing.T) {
	_, stdout, _ := invoke("--config", fixture("commerce.json"),
		"--bind", "customers from India", "--embedder", "deterministic", "--json")
	var out struct {
		Query string `json:"query"`
		Bound bool   `json:"bound"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if out.Query == "" || (!out.Bound && out.Error == "") {
		t.Errorf("fields missing: %+v", out)
	}
}

// The redaction sweep, as for --rank: misspellings retrieve values the user
// did not type, so any rendering of them is a leak. Text and JSON, three
// fixtures.
//
// It needs REFUSED sentences too. A successful binding prints field names and
// redacted values, never corpus ids; ids appear only in the reading shown on
// a refusal. Without "Freedon Zorblatt" (refused on the unexplained word, with
// the Freedonia alias in its reading), disabling corpus.DisplayID entirely
// did not fail this sweep — found by mutation.
func TestBindLeaksNoConfigValueByDefault(t *testing.T) {
	cases := map[string][]string{
		"commerce.json":  {"customers from Indi", "customers who bought laptop", "customers from India who spent more than 5"},
		"ambiguity.json": {"customers from Freedon", "payments that bounce", "customers from Freedonia", "customers from Freedon Zorblatt"},
		"multipath.json": {"orders shipped to customers in Ruritani", "customers in Graustar"},
	}
	for cfg, sentences := range cases {
		values := fixtureValues(t, cfg)
		for _, s := range sentences {
			for _, mode := range [][]string{nil, {"--json"}} {
				_, stdout, stderr := invoke(append([]string{"--config", fixture(cfg), "--bind", s,
					"--embedder", "deterministic", "--top-k", "20"}, mode...)...)
				if l := leaked(stdout+stderr, s, values); len(l) > 0 {
					t.Errorf("%s %q %v leaks %v", cfg, s, mode, l)
				}
			}
		}
	}
}

// A refusal's error names positions, never the user's words, by default.
func TestBindErrorWithholdsUserText(t *testing.T) {
	_, _, stderr := invoke("--config", fixture("commerce.json"),
		"--bind", "customers from Zorblatt", "--embedder", "deterministic")
	if strings.Contains(stderr, "Zorblatt") {
		t.Errorf("default error quotes user text (decision 013):\n%s", stderr)
	}
	_, _, debug := invoke("--config", fixture("commerce.json"),
		"--bind", "customers from Zorblatt", "--embedder", "deterministic", "--debug")
	if !strings.Contains(debug, "Zorblatt") && strings.Contains(debug, "tokens[") {
		t.Errorf("--debug should reveal the phrase:\n%s", debug)
	}
}
