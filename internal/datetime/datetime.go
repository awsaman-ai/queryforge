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

// PeriodBounds resolves a calendar period into the half-open instant range
// [start, end) it covers, as seen from now in loc.
//
// A period counts whole calendar units away from the one now falls in: 0 is
// the current day/week/month, -1 the previous one, and from/to are BOTH
// inclusive. So (day, -1, -1) is yesterday, (day, -29, 0) is the 30 calendar
// days ending with today, and (month, -1, -1) is last month.
//
// This is deliberately not relative_date. A relative_date is one instant a
// fixed distance from now, so "last 30 days" asked at 14:37 started at 14:37
// thirty days ago and gave a different answer at 09:00 and at 23:00; and
// "yesterday" or "this year" could not be expressed at all, leaving the model
// to do calendar arithmetic that nothing checked. A period's boundaries fall
// on calendar lines in the configured timezone, so the same question asked
// any time on the same day selects the same rows.
//
// The range is half-open on purpose. `end` is the first instant of the unit
// AFTER `to`, compared with `<`, so a row stamped 23:59:59.999 on the last day
// is included and midnight of the next day is not — an inclusive
// "<= 2026-08-31" on a timestamp column silently drops almost all of Aug 31.
//
// Weeks start on Monday (ISO 8601). Both instants are returned in UTC, the
// zone every generator emits. The caller must have checked unit and the
// magnitude of from/to (IsRelativeUnit, MaxRelativeAmount) and that from <= to;
// an unknown unit falls back to day here rather than panicking.
func PeriodBounds(now time.Time, loc *time.Location, unit string, from, to int) (start, end time.Time) {
	if loc == nil { // an unfinalized Config has no location: behave like the default
		loc = time.UTC
	}
	anchor := StartOfUnit(now.In(loc), unit) // first instant of the unit now falls in
	start = ShiftUnits(anchor, unit, from)   // first instant of the `from` unit
	end = ShiftUnits(anchor, unit, to+1)     // first instant of the unit after `to`
	return start.UTC(), end.UTC()
}

// StartOfUnit truncates t, in t's own location, to the first instant of the
// calendar unit containing it. Built with time.Date rather than Truncate,
// because Truncate works on absolute time since the zero instant and so cuts
// at UTC boundaries — in Asia/Kolkata that puts "midnight" at 05:30.
func StartOfUnit(t time.Time, unit string) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch unit {
	case "minute":
		return time.Date(y, m, d, t.Hour(), t.Minute(), 0, 0, loc)
	case "hour":
		return time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
	case "week":
		// Weekday counts Sunday as 0; (w+6)%7 is days since the most recent Monday.
		back := (int(t.Weekday()) + 6) % 7
		return time.Date(y, m, d-back, 0, 0, 0, 0, loc)
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case "year":
		return time.Date(y, time.January, 1, 0, 0, 0, 0, loc)
	default: // "day", and the defensive fallback for an unchecked unit
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
}

// ShiftUnits moves a unit-start instant by n whole units.
//
// Day, week, month and year shift on the calendar (time.Date normalizes
// "August 32" into September 1), so a daylight-saving change in between still
// lands on local midnight rather than 23:00 or 01:00. Minute and hour shift on
// the absolute clock instead: across a DST change a local wall-clock hour can
// occur twice or not at all, and a fixed 60 minutes is the only reading of
// "the last 3 hours" that cannot skip or double-count an hour.
func ShiftUnits(t time.Time, unit string, n int) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch unit {
	case "minute":
		return t.Add(time.Duration(n) * time.Minute)
	case "hour":
		return t.Add(time.Duration(n) * time.Hour)
	case "week":
		return time.Date(y, m, d+7*n, 0, 0, 0, 0, loc)
	case "month":
		return time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, loc)
	case "year":
		return time.Date(y+n, time.January, 1, 0, 0, 0, 0, loc)
	default: // "day"
		return time.Date(y, m, d+n, 0, 0, 0, 0, loc)
	}
}
