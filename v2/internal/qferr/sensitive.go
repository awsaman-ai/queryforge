package qferr

import "fmt"

// This file defines the experiment's redaction contract. It lives in qferr
// because every phase already imports this package for its errors, and the
// contract has to be impossible to miss.
//
// THE CONTRACT
//
//	Default output may name STRUCTURE.  Entity, field, table, column, operator,
//	aggregation, type and rule names, and positions in the config. Without these
//	a message cannot be acted on, and they describe the schema's shape rather
//	than anything stored in it.
//
//	Default output may NOT contain VALUES. Enum values, value-alias keys and
//	targets, literals extracted from a user sentence, and anything else drawn
//	from the data domain. A config's aliases can map real customer names,
//	account numbers or internal codes, and an error message is the easiest place
//	for those to escape into a log aggregator.
//
//	Values appear only under an explicit debug mode, chosen per invocation.
//
// The mechanism is a type, not a rule to remember: wrap a value in Sensitive and
// it redacts itself everywhere by default. The failure mode of forgetting to
// call something is a leak; the failure mode of forgetting to wrap is caught by
// TestNoFixtureLeaksValuesByDefault, which sweeps every fixture's own values
// against the rendered output.
//
// Phase 2 onward: the semantic corpus is built FROM config values — enum values
// and aliases become embedded text. Nothing in that pipeline may assume a value
// is safe to put in an error just because the corpus already holds it.

// Placeholder is what a redacted value renders as.
const Placeholder = "<redacted>"

// Sensitive marks a value that must not appear in default output.
//
// It is a string type with a String method, so the redacted form is what every
// fmt verb produces unless something deliberately reveals it. That ordering
// matters: the safe rendering is the one you get by accident.
type Sensitive string

// String renders the placeholder. fmt consults it for %v, %s and %q, so all
// three redact.
func (s Sensitive) String() string { return Placeholder }

// Reveal returns the underlying value. Every call site is a deliberate decision
// to expose it, and should be reachable only from a debug path.
func (s Sensitive) Reveal() string { return string(s) }

// Format renders a message with Sensitive arguments redacted. This is the
// default rendering and what any user-facing message must use.
func Format(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// FormatRevealed renders a message with Sensitive arguments replaced by their
// real values.
//
// DEBUG ONLY. Output from this function must never reach default stdout/stderr,
// a metric label, or a log line that is on by default.
func FormatRevealed(format string, args ...any) string {
	revealed := make([]any, len(args))
	for i, arg := range args {
		if s, ok := arg.(Sensitive); ok {
			revealed[i] = s.Reveal()
			continue
		}
		revealed[i] = arg
	}
	return fmt.Sprintf(format, revealed...)
}

// HasSensitive reports whether any argument is a redacted value. Used by tests
// that assert a message pair actually differs between the two renderings — a
// debug rendering identical to the default one usually means a value was
// interpolated into the format string instead of being passed as an argument.
func HasSensitive(args ...any) bool {
	for _, arg := range args {
		if _, ok := arg.(Sensitive); ok {
			return true
		}
	}
	return false
}
