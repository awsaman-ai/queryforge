package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// CLI tests for --query and --e2e: the output contract and redaction. The
// deterministic embedder is not semantic, so these accept either outcome and
// check the contract of each; exact end-to-end SQL is internal/pipeline's.

func queryArgs(q string, extra ...string) []string {
	return append([]string{"--config", fixture("commerce.json"), "--query", q,
		"--embedder", "deterministic", "--now", "2026-09-21T12:00:00Z"}, extra...)
}

func TestQueryJSONContract(t *testing.T) {
	for _, q := range []string{"show customers from India", "zzz qqq"} {
		code, stdout, _ := invoke(queryArgs(q, "--json")...)
		var out struct {
			OK     bool   `json:"ok"`
			SQL    string `json:"sql"`
			Error  string `json:"error"`
			Stage  string `json:"stage"`
			Params []struct {
				Index int `json:"index"`
			} `json:"params"`
			Telemetry *struct{} `json:"telemetry"`
		}
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("%q: not JSON: %v\n%s", q, err, stdout)
		}
		if out.OK != (code == exitOK) || out.OK != (out.SQL != "") || out.OK == (out.Error != "") || out.Stage == "" {
			t.Errorf("%q: exit %d, inconsistent JSON %+v", q, code, out)
		}
		if out.Telemetry != nil {
			t.Errorf("%q: telemetry without --debug", q)
		}
	}
}

// Telemetry appears only under --debug, on stderr, as one JSON line built
// from pipeline.Stats — never with the user's words.
func TestQueryTelemetryIsStructuredAndRedacted(t *testing.T) {
	_, _, stderr := invoke(queryArgs("customers from Zorblattia")...)
	if strings.Contains(stderr, "telemetry:") {
		t.Error("telemetry printed without --debug")
	}
	_, _, stderr = invoke(queryArgs("customers from Zorblattia", "--debug")...)
	var line string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "telemetry: ") {
			line = strings.TrimPrefix(l, "telemetry: ")
		}
	}
	var stats map[string]any
	if err := json.Unmarshal([]byte(line), &stats); err != nil {
		t.Fatalf("no telemetry JSON: %v\n%s", err, stderr)
	}
	if strings.Contains(line, "Zorblattia") || stats["total_latency_ms"] == nil || stats["phase"] == nil {
		t.Errorf("telemetry = %s", line)
	}
}

func TestQueryExplainShowsTheStages(t *testing.T) {
	code, stdout, _ := invoke(queryArgs("show customers from India", "--explain")...)
	if !strings.Contains(stdout, "explain:") || !strings.Contains(stdout, "reading:") {
		t.Errorf("explain output:\n%s", stdout)
	}
	if code == exitOK && (!strings.Contains(stdout, "binding:") || !strings.Contains(stdout, "plan:")) {
		t.Errorf("a successful explain must show binding and plan:\n%s", stdout)
	}
	if code == exitFailure && !strings.Contains(stdout, "refused at:") {
		t.Errorf("a refused explain must say where:\n%s", stdout)
	}
}

// The refusal on stderr never quotes the user's words (decision 013).
func TestQueryRefusalDoesNotQuoteTheUser(t *testing.T) {
	code, _, stderr := invoke(queryArgs("customers from Zorblattia")...)
	if code == exitFailure && strings.Contains(stderr, "Zorblattia") {
		t.Errorf("stderr quotes the user: %s", stderr)
	}
}

func TestE2ERefusesAMismatchedConfig(t *testing.T) {
	code, _, stderr := invoke("--config", fixture("multipath.json"), "--embedder", "deterministic",
		"--e2e", filepath.Join("..", "..", "testdata", "queries", "phase7-e2e.json"))
	if code != exitFailure || !strings.HasPrefix(stderr, "INVALID_INPUT") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}
