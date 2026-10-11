package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
)

// This file enforces the redaction contract in internal/qferr/sensitive.go.
//
// The mechanism — wrapping a value in qferr.Sensitive — only protects values
// somebody remembered to wrap. These tests close that gap generically: they read
// the VALUES out of each fixture's own JSON and assert none of them appear in
// default output, whatever the validator happened to say. A rule added in a
// later phase that interpolates a value into a message fails here without anyone
// having to write a test for that specific rule.

// configValues extracts the data-domain values from a config file: enum values
// and value-alias keys and targets. It deliberately does NOT collect entity,
// field, table or column names — those are structure, which the contract allows
// in default output.
func configValues(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	// Decoded loosely rather than into Config, so that a fixture which is
	// invalid in some structural way still yields its values.
	var raw struct {
		Fields []struct {
			EnumValues   []string          `json:"enumValues"`
			ValueAliases map[string]string `json:"valueAliases"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		// A deliberately malformed-JSON fixture has no extractable values.
		return nil
	}

	var values []string
	for _, f := range raw.Fields {
		values = append(values, f.EnumValues...)
		for alias, target := range f.ValueAliases {
			values = append(values, alias, target)
		}
	}

	out := values[:0]
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// containsValue reports whether text mentions value as a whole word.
//
// Whole-word and case-sensitive on purpose. A naive substring check would
// false-positive on short values — the alias target "IN" occurs inside the
// ordinary English "... is not declared in enumValues" — and a test that cries
// wolf gets weakened until it catches nothing.
//
// The remaining limitation is deliberate and must not be papered over: a config
// value that IS an ordinary English word will still false-positive, because
// nothing distinguishes it from prose. The fix is for fixtures to use
// distinctive values, not for this check to skip short ones — skipping would
// blind it to exactly the short codes ("IN", "US") that configs are full of.
// invalid-enum.json originally used ["a","b"] and tripped on the article in
// "withheld a config value"; the fixture changed, not the check.
func containsValue(text, value string) bool {
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(value) + `\b`)
	if err != nil {
		return strings.Contains(text, value)
	}
	return re.MatchString(text)
}

// TestNoFixtureLeaksValuesByDefault sweeps every invalid fixture: whatever the
// validator says about it, no value from the file may appear in the default
// rendering.
func TestNoFixtureLeaksValuesByDefault(t *testing.T) {
	for _, path := range invalidFixtures(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			_, err := Load(path)
			if err == nil {
				t.Fatal("fixture was accepted")
			}
			rendered := err.Error()

			for _, value := range configValues(t, path) {
				if containsValue(rendered, value) {
					t.Errorf("default output leaks the config value %q:\n%s", value, rendered)
				}
			}
		})
	}
}

// The mirror image: a message that withheld something must actually show it
// under debug. Without this, the contract could be satisfied by a validator that
// says nothing useful in either mode.
func TestDebugRenderingRevealsWithheldValues(t *testing.T) {
	// Fixtures whose violations carry values, and one value each that the
	// revealed rendering must name.
	cases := map[string][]string{
		"invalid-value-alias.json": {"India", "india", "Nowhere", "disabled", "suspended"},
		"invalid-enum.json":        {"Gold"},
	}

	for file, expected := range cases {
		t.Run(file, func(t *testing.T) {
			_, err := Load(fixture(file))
			if err == nil {
				t.Fatal("fixture was accepted")
			}
			ve, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("not a ValidationError: %v", err)
			}

			if ve.RedactedValueCount() == 0 {
				t.Fatal("this fixture should have withheld at least one value")
			}

			revealed := ve.ErrorRevealed()
			for _, value := range expected {
				if !containsValue(revealed, value) {
					t.Errorf("debug output should name %q:\n%s", value, revealed)
				}
			}
			if strings.Contains(revealed, qferr.Placeholder) {
				t.Errorf("debug output still contains a placeholder:\n%s", revealed)
			}
		})
	}
}

// The default message must say that something was withheld. A redacted message
// that gives no hint is indistinguishable from a vague one, and the difference
// decides whether anyone thinks to re-run with --debug.
func TestDefaultOutputAnnouncesRedaction(t *testing.T) {
	_, err := Load(fixture("invalid-value-alias.json"))
	if err == nil {
		t.Fatal("fixture was accepted")
	}
	msg := err.Error()

	if !strings.Contains(msg, qferr.Placeholder) {
		t.Errorf("redacted values should be visibly redacted:\n%s", msg)
	}
	if !strings.Contains(msg, "--debug") {
		t.Errorf("the message should say how to see the withheld values:\n%s", msg)
	}
}

// A fixture with nothing sensitive must not carry the "withheld" notice, or the
// notice becomes noise that people learn to ignore.
func TestNoRedactionNoticeWhenNothingWithheld(t *testing.T) {
	_, err := Load(fixture("invalid-relationship.json"))
	if err == nil {
		t.Fatal("fixture was accepted")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("not a ValidationError: %v", err)
	}
	if ve.RedactedValueCount() != 0 {
		t.Fatalf("this fixture has no value-bearing violations, got %d", ve.RedactedValueCount())
	}
	if strings.Contains(err.Error(), "withheld") {
		t.Errorf("no values were withheld, so there should be no notice:\n%s", err.Error())
	}
}

// Both renderings must stay deterministic. Revealing values must not change the
// order of violations, or a debug run would not line up with the default one it
// is meant to explain.
func TestBothRenderingsAreDeterministic(t *testing.T) {
	const file = "invalid-value-alias.json"

	_, err := Load(fixture(file))
	if err == nil {
		t.Fatal("fixture was accepted")
	}
	first := err.(*ValidationError)

	for i := 1; i < 20; i++ {
		_, err := Load(fixture(file))
		if err == nil {
			t.Fatal("fixture was accepted")
		}
		next := err.(*ValidationError)
		if next.Error() != first.Error() {
			t.Fatalf("default rendering varies between runs")
		}
		if next.ErrorRevealed() != first.ErrorRevealed() {
			t.Fatalf("debug rendering varies between runs")
		}
	}

	// The two renderings must describe the same violations in the same order.
	if got, want := strings.Count(first.ErrorRevealed(), "\n  - "), len(first.Violations); got != want {
		t.Errorf("debug rendering lists %d violations, want %d", got, want)
	}
}

// invalidFixtures lists every invalid-*.json fixture, so a fixture added later
// is swept automatically rather than needing to be registered here.
func invalidFixtures(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", "testdata", "configs", "invalid-*.json"))
	if err != nil {
		t.Fatalf("globbing fixtures: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no invalid fixtures found; the sweep is checking nothing")
	}
	return matches
}
