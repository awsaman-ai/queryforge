package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
)

// These use --embedder deterministic so the suite runs in a fresh checkout with
// no model download. They test the CLI's plumbing and its redaction, not
// retrieval quality; quality is measured against the real model and recorded in
// docs/phase-2-results.md.

func TestSearchReportsMatches(t *testing.T) {
	code, stdout, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--search", "customer spending",
		"--embedder", "deterministic",
		"--top-k", "5",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	for _, want := range []string{"query:", "model:", "strategy:", "rank", "score"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}

	// At most --top-k result rows.
	rows := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 ") ||
			strings.HasPrefix(line, "3 ") || strings.HasPrefix(line, "4 ") || strings.HasPrefix(line, "5 ") {
			rows++
		}
	}
	if rows == 0 {
		t.Errorf("no result rows:\n%s", stdout)
	}
	if rows > 5 {
		t.Errorf("--top-k 5 produced %d rows", rows)
	}
}

// The non-semantic provider must announce itself, or a mechanism check gets
// pasted into a results table.
func TestDeterministicEmbedderWarns(t *testing.T) {
	_, _, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--search", "customer spending",
		"--embedder", "deterministic",
	)
	if !strings.Contains(stderr, "NOT semantic") {
		t.Errorf("the hashing embedder should warn loudly:\n%s", stderr)
	}
}

// The redaction contract applied to retrieval. A value alias is among the most
// likely things a query about a value retrieves, so this is a common path.
func TestSearchRedactsValueMatches(t *testing.T) {
	code, stdout, _ := invoke(
		"--config", fixture("commerce.json"),
		"--search", "India",
		"--embedder", "deterministic",
		"--top-k", "25",
	)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "VALUE_ALIAS") {
		t.Skip("no value entry in the top 25 under the hashing embedder")
	}

	for _, value := range []string{"India", "United Kingdom", "suspended", "laptop"} {
		// The entry ID legitimately contains the value, and the id is
		// structure — it names which config object matched. What must not
		// appear is the matched TEXT column.
		for _, line := range strings.Split(stdout, "\n") {
			if !strings.Contains(line, "VALUE_ALIAS") {
				continue
			}
			cols := strings.Fields(line)
			matched := cols[len(cols)-1]
			if matched == value {
				t.Errorf("the matched-text column shows the config value %q:\n%s", value, line)
			}
		}
	}
	if !strings.Contains(stdout, qferr.Placeholder) {
		t.Errorf("value matches should render redacted:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--debug") {
		t.Errorf("output should say how to see redacted matches:\n%s", stdout)
	}
}

func TestSearchRevealsValueMatchesUnderDebug(t *testing.T) {
	_, stdout, _ := invoke(
		"--config", fixture("commerce.json"),
		"--search", "India",
		"--embedder", "deterministic",
		"--top-k", "25",
		"--debug",
	)
	if !strings.Contains(stdout, "VALUE_ALIAS") {
		t.Skip("no value entry in the top 25 under the hashing embedder")
	}
	if strings.Contains(stdout, qferr.Placeholder) {
		t.Errorf("--debug should reveal matched values:\n%s", stdout)
	}
	// And debug adds timing, which is what the Phase 9 work will read.
	if !strings.Contains(stdout, "timing:") {
		t.Errorf("--debug should report timings:\n%s", stdout)
	}
}

func TestSearchJSON(t *testing.T) {
	code, stdout, _ := invoke(
		"--config", fixture("commerce.json"),
		"--search", "customer spending",
		"--embedder", "deterministic",
		"--json",
	)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}

	var out struct {
		Query string `json:"query"`
		Model struct {
			Dim      int  `json:"dim"`
			Semantic bool `json:"semantic"`
		} `json:"model"`
		Strategy string `json:"strategy"`
		Matches  []struct {
			Rank     int     `json:"rank"`
			Score    float64 `json:"score"`
			Kind     string  `json:"kind"`
			ID       string  `json:"id"`
			Matched  string  `json:"matched"`
			Redacted bool    `json:"redacted"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if out.Query != "customer spending" {
		t.Errorf("query = %q", out.Query)
	}
	if out.Model.Semantic {
		t.Error("the hashing provider must report semantic=false in JSON too")
	}
	if len(out.Matches) == 0 {
		t.Fatal("no matches in JSON output")
	}
	if out.Matches[0].Rank != 1 {
		t.Errorf("first match rank = %d, want 1", out.Matches[0].Rank)
	}
	for _, m := range out.Matches {
		if m.Redacted && m.Matched != qferr.Placeholder {
			t.Errorf("match %s is flagged redacted but shows %q", m.ID, m.Matched)
		}
	}
}

func TestSearchRejectsUnknownStrategy(t *testing.T) {
	code, _, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--search", "x",
		"--strategy", "telepathy",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "unknown strategy") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestSearchRejectsUnknownEmbedder(t *testing.T) {
	code, _, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--search", "x",
		"--embedder", "telepathy",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "unknown embedder") {
		t.Errorf("stderr = %q", stderr)
	}
}

// Index build and reload through the CLI, which is how the cache strategy in
// section 10 is actually used.
func TestBuildIndexAndReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commerce.index.json")

	code, stdout, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--build-index", "--index", path,
		"--embedder", "deterministic",
	)
	if code != exitOK {
		t.Fatalf("build exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "index written") {
		t.Errorf("stdout = %q", stdout)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index file was not created: %v", err)
	}

	code, stdout, stderr = invoke(
		"--config", fixture("commerce.json"),
		"--search", "customer spending",
		"--index", path,
		"--embedder", "deterministic",
	)
	if code != exitOK {
		t.Fatalf("search exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "index loaded from") {
		t.Errorf("the cached index should have been reused:\n%s", stdout)
	}
}

// A cached index built from a different config must be refused, not used. This
// is the dangerous case: it parses cleanly and would retrieve under names the
// config no longer has.
func TestStaleIndexIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.index.json")

	if code, _, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--build-index", "--index", path,
		"--embedder", "deterministic",
	); code != exitOK {
		t.Fatalf("build failed: %s", stderr)
	}

	code, _, stderr := invoke(
		"--config", fixture("customers.json"),
		"--search", "spending",
		"--index", path,
		"--embedder", "deterministic",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "stale") {
		t.Errorf("stderr = %q, want a staleness message", stderr)
	}
}

func TestBuildIndexNeedsAPath(t *testing.T) {
	code, _, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--build-index",
		"--embedder", "deterministic",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "--index") {
		t.Errorf("stderr = %q, should point at --index", stderr)
	}
}

func TestProbeReportsAccuracy(t *testing.T) {
	code, stdout, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--probe", filepath.Join("..", "..", "testdata", "queries", "phase2-probes.tsv"),
		"--embedder", "deterministic",
	)
	if code != exitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"Top-1:", "Top-3:", "| query | expected |"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("probe output is missing %q:\n%s", want, stdout[:min(len(stdout), 600)])
		}
	}
	// The non-semantic banner must appear in the report itself, not only on
	// stderr — the report is what gets copied into a document.
	if !strings.Contains(stdout, "NON-SEMANTIC") {
		t.Errorf("a probe run on the hashing provider must mark itself:\n%s", stdout[:min(len(stdout), 400)])
	}
}

func TestProbeRejectsMissingFile(t *testing.T) {
	code, _, stderr := invoke(
		"--config", fixture("commerce.json"),
		"--probe", "/nonexistent/probes.tsv",
		"--embedder", "deterministic",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "probe file") {
		t.Errorf("stderr = %q", stderr)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
