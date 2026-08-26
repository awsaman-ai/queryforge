// Package datetime holds the one date-parsing rule the whole library shares.
//
// It is its own leaf package for a layering reason, not a stylistic one. The
// Mongo and Elasticsearch generators parse date literals; the validator refuses
// a date literal the generators would not parse; and the Elasticsearch routing
// checks in internal/config refuse one at load time. Those three sit at three
// different levels of the dependency graph, and the lowest of them is
// internal/config — so the shared rule has to live below even that, or the
// packages that agree on it could not all import it.
//
// Sharing one implementation is the whole point: a validator that admitted a
// date the generator then handed back as a bare string is exactly the class of
// bug this arrangement prevents.
package datetime

import "time"

// ParseDate parses common date layouts into a UTC time.Time, returning the
// original string when it matches none (so unexpected input degrades safely).
func ParseDate(s string) any {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return s
}

// IsDateString reports whether s parses under one of the layouts the generators
// accept. Sharing it with ParseDate is what keeps the validator from admitting a
// date the Mongo generator would then hand back as a bare string.
func IsDateString(s string) bool {
	_, ok := ParseDate(s).(time.Time)
	return ok
}

// RelativeUnits is the closed set of units a relative_date may carry. It is the
// same list the system prompt shows the model, and it is what the validator
// enforces (validateRelativeDate) so a generator never has to guess.
var RelativeUnits = []string{"minute", "hour", "day", "week", "month", "year"}

// MaxRelativeAmount bounds the magnitude of a relative_date amount.
//
// Unbounded, `amount` is a plain int off the wire. time.Duration(amount) *
// time.Minute overflows int64 nanoseconds past roughly 1.5e11 minutes and wraps
// to a time on the wrong side of now; AddDate with a huge amount lands on a year
// outside any column's range, which drivers reject at bind time with an opaque
// error. A million of any unit is ~1e6 years at the coarse end and ~2 years at
// the fine end — far past any real question, far short of either failure.
const MaxRelativeAmount = 1_000_000

// IsRelativeUnit reports whether unit is one of the known units.
func IsRelativeUnit(unit string) bool {
	for _, u := range RelativeUnits {
		if u == unit {
			return true
		}
	}
	return false
}
