package qferr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestSensitiveRedactsUnderEveryVerb is the heart of the contract. Sensitive is
// a string type, so without its String method %s would print the raw value. fmt
// consults Stringer for %v, %s and %q, and all three must redact — a call site
// that reaches for %q to quote a value must not thereby reveal it.
func TestSensitiveRedactsUnderEveryVerb(t *testing.T) {
	secret := Sensitive("acme-corp-internal")

	for _, verb := range []string{"%v", "%s", "%q"} {
		got := fmt.Sprintf(verb, secret)
		if strings.Contains(got, "acme-corp-internal") {
			t.Errorf("%s leaked the value: %s", verb, got)
		}
		if !strings.Contains(got, Placeholder) {
			t.Errorf("%s = %s, want the placeholder", verb, got)
		}
	}
}

// The safe rendering must be the one a caller gets by accident. Anything that
// reveals has to be reached for deliberately.
func TestFormatRedactsAndFormatRevealedDoesNot(t *testing.T) {
	const format = "value alias %s points at %s"
	alias, target := Sensitive("premium"), Sensitive("enterprise")

	safe := Format(format, alias, target)
	if strings.Contains(safe, "premium") || strings.Contains(safe, "enterprise") {
		t.Errorf("Format leaked a value: %s", safe)
	}
	if strings.Count(safe, Placeholder) != 2 {
		t.Errorf("Format = %q, want both values redacted", safe)
	}

	revealed := FormatRevealed(format, alias, target)
	if !strings.Contains(revealed, "premium") || !strings.Contains(revealed, "enterprise") {
		t.Errorf("FormatRevealed = %q, want both values shown", revealed)
	}
	if strings.Contains(revealed, Placeholder) {
		t.Errorf("FormatRevealed = %q, should not redact", revealed)
	}
}

// Structural arguments — field names, types, rule names — pass through both
// renderings unchanged. Redacting them would make messages unactionable, which
// is the failure mode that pushes people to turn redaction off entirely.
func TestStructuralArgumentsAreNeverRedacted(t *testing.T) {
	const format = "field %q of type %q rejected value %s"
	for _, render := range []func(string, ...any) string{Format, FormatRevealed} {
		got := render(format, "customer.status", "enum", Sensitive("suspended"))
		if !strings.Contains(got, "customer.status") || !strings.Contains(got, "enum") {
			t.Errorf("structural argument was altered: %s", got)
		}
	}
}

func TestHasSensitive(t *testing.T) {
	if !HasSensitive("field", Sensitive("x"), 3) {
		t.Error("HasSensitive should find a wrapped value")
	}
	if HasSensitive("field", "plain", 3) {
		t.Error("HasSensitive should not report structural arguments")
	}
	if HasSensitive() {
		t.Error("HasSensitive on no arguments should be false")
	}
}

// Reveal is the single documented escape hatch, so it must actually work —
// otherwise debug mode would show placeholders and someone would "fix" it by
// removing the wrapper.
func TestRevealReturnsTheValue(t *testing.T) {
	if got := Sensitive("india").Reveal(); got != "india" {
		t.Errorf("Reveal = %q, want %q", got, "india")
	}
}

func TestJSONRedactsByDefaultAndRevealsOnDemand(t *testing.T) {
	err := Wrap(CodeConfigError, errors.New("line 3: acme-corp-internal"), "config is invalid")

	safe, jerr := err.JSON()
	if jerr != nil {
		t.Fatalf("JSON: %v", jerr)
	}
	if strings.Contains(string(safe), "acme-corp-internal") {
		t.Errorf("JSON leaked the cause: %s", safe)
	}

	revealed, jerr := err.JSONRevealed("value alias premium points at enterprise")
	if jerr != nil {
		t.Fatalf("JSONRevealed: %v", jerr)
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
			Cause   string `json:"cause"`
		} `json:"error"`
	}
	if err := json.Unmarshal(revealed, &out); err != nil {
		t.Fatalf("JSONRevealed is not valid JSON: %v\n%s", err, revealed)
	}
	if !strings.Contains(out.Error.Detail, "premium") {
		t.Errorf("detail = %q, want the revealed values", out.Error.Detail)
	}
	if !strings.Contains(out.Error.Cause, "acme-corp-internal") {
		t.Errorf("cause = %q, want the wrapped cause", out.Error.Cause)
	}
	if out.Error.Code != string(CodeConfigError) {
		t.Errorf("code = %q, want %q", out.Error.Code, CodeConfigError)
	}
}

// An error carrying no detail must not sprout empty fields; the debug rendering
// of a plain error should still read cleanly.
func TestJSONRevealedOmitsEmptyFields(t *testing.T) {
	enc, jerr := New(CodeInvalidInput, "--config is required").JSONRevealed("")
	if jerr != nil {
		t.Fatalf("JSONRevealed: %v", jerr)
	}
	if strings.Contains(string(enc), "detail") || strings.Contains(string(enc), "cause") {
		t.Errorf("empty debug fields should be omitted: %s", enc)
	}
}
