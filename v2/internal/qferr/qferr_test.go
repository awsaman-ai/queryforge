package qferr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCodeOfFindsCodeThroughWrapping(t *testing.T) {
	inner := New(CodeAmbiguousMatch, "two fields matched")
	outer := fmt.Errorf("while planning: %w", inner)

	if got := CodeOf(outer); got != CodeAmbiguousMatch {
		t.Errorf("CodeOf = %s, want %s", got, CodeAmbiguousMatch)
	}
}

// An error that never got a code must not be quietly filed under a user-facing
// category — an unclassified failure is an engine bug and should read as one.
func TestCodeOfDefaultsToInternal(t *testing.T) {
	if got := CodeOf(errors.New("something went wrong")); got != CodeInternal {
		t.Errorf("CodeOf = %s, want %s", got, CodeInternal)
	}
}

func TestWrapKeepsCauseUnwrappable(t *testing.T) {
	cause := errors.New("unexpected end of JSON input")
	err := Wrap(CodeConfigError, cause, "config file is not valid JSON")

	if !errors.Is(err, cause) {
		t.Error("errors.Is should find the wrapped cause")
	}
	if strings.Contains(err.Message, cause.Error()) {
		t.Error("the cause must stay out of the redaction-safe message")
	}
}

func TestJSONCarriesOnlyCodeAndMessage(t *testing.T) {
	err := Wrap(CodeConfigError, errors.New("line 3: secret-looking content"), "config file is not valid JSON")
	enc, jerr := err.JSON()
	if jerr != nil {
		t.Fatalf("JSON: %v", jerr)
	}
	if strings.Contains(string(enc), "secret-looking") {
		t.Errorf("the cause leaked into JSON output: %s", enc)
	}
	if !strings.Contains(string(enc), "CONFIG_ERROR") {
		t.Errorf("JSON output is missing the code: %s", enc)
	}
}
