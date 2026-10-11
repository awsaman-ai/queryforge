package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnalyzeNeedsNoConfig(t *testing.T) {
	// Extraction is config-independent by design: what a span MEANS needs the
	// schema, what a span IS does not. Requiring --config here would imply
	// otherwise and make the mode useless for debugging a config problem.
	code, stdout, stderr := invoke("--analyze", "customers who spent more than 50000")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	for _, want := range []string{"input:", "tokens:", "candidate spans"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestAnalyzeReportsNumbersAndDates(t *testing.T) {
	_, stdout, _ := invoke("--analyze", "customers who spent over fifty thousand last year")

	if !strings.Contains(stdout, "50000") {
		t.Errorf("the spelled number should be reported as a value:\n%s", stdout)
	}
	if !strings.Contains(stdout, "spelled") {
		t.Errorf("a number written in words should be marked spelled:\n%s", stdout)
	}
	if !strings.Contains(stdout, "calendar year") {
		t.Errorf("'last year' should be reported as a calendar year:\n%s", stdout)
	}
}

func TestAnalyzeReportsConnectives(t *testing.T) {
	_, stdout, _ := invoke("--analyze", "customers from India and orders above 100")
	if !strings.Contains(stdout, "connectives:") || !strings.Contains(stdout, "and") {
		t.Errorf("the conjunction should be reported:\n%s", stdout)
	}
}

func TestAnalyzeDebugShowsTokens(t *testing.T) {
	_, quiet, _ := invoke("--analyze", "customers from India")
	if strings.Contains(quiet, "tokens:\n") && strings.Contains(quiet, "(stop)") {
		t.Errorf("per-token detail should need --debug:\n%s", quiet)
	}

	_, loud, _ := invoke("--analyze", "customers from India", "--debug")
	if !strings.Contains(loud, "(stop)") {
		t.Errorf("--debug should mark function words:\n%s", loud)
	}
}

func TestAnalyzeJSON(t *testing.T) {
	code, stdout, _ := invoke("--analyze", "customers who spent more than 50000 last year", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}

	var out struct {
		Input      string `json:"input"`
		TokenCount int    `json:"tokenCount"`
		Numbers    []struct {
			Text  string  `json:"text"`
			Value float64 `json:"value"`
		} `json:"numbers"`
		Dates []struct {
			Unit   string `json:"unit"`
			Anchor string `json:"anchor"`
		} `json:"dates"`
		Spans []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"spans"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if out.TokenCount == 0 || len(out.Spans) == 0 {
		t.Fatalf("empty analysis: %+v", out)
	}
	if len(out.Numbers) != 1 || out.Numbers[0].Value != 50000 {
		t.Errorf("numbers = %+v, want one value of 50000", out.Numbers)
	}
	if len(out.Dates) != 1 || out.Dates[0].Unit != "year" {
		t.Errorf("dates = %+v, want one year", out.Dates)
	}
}

func TestAnalyzeRejectsEmptyInput(t *testing.T) {
	code, _, stderr := invoke("--analyze", "   ")
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "needs a sentence") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestAnalyzeRespectsMaxSpanTokens(t *testing.T) {
	_, wide, _ := invoke("--analyze", "customers who spent more than fifty thousand dollars")
	_, narrow, _ := invoke("--analyze", "customers who spent more than fifty thousand dollars", "--max-span-tokens", "2")

	wideCount := strings.Count(wide, "tokens[")
	narrowCount := strings.Count(narrow, "tokens[")
	if narrowCount >= wideCount {
		t.Errorf("a smaller span limit should produce fewer candidates: %d vs %d", narrowCount, wideCount)
	}
}

// Extraction is deterministic, which every later phase depends on.
func TestAnalyzeIsDeterministic(t *testing.T) {
	const sentence = "show the top 10 customers from India who spent more than 50000 last year"
	_, first, _ := invoke("--analyze", sentence, "--json")
	for i := 0; i < 5; i++ {
		_, again, _ := invoke("--analyze", sentence, "--json")
		if again != first {
			t.Fatalf("analysis varies between runs")
		}
	}
}
