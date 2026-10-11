package nlp

import (
	"strconv"
	"time"
)

// DateUnit is the granularity of a date expression.
type DateUnit string

const (
	UnitDay     DateUnit = "day"
	UnitWeek    DateUnit = "week"
	UnitMonth   DateUnit = "month"
	UnitQuarter DateUnit = "quarter"
	UnitYear    DateUnit = "year"
)

// DateAnchor says how a date expression is positioned relative to now.
type DateAnchor string

const (
	// AnchorRolling is a window measured backwards from this instant:
	// "last 6 months" is the 6 months up to now, not the 6 previous calendar
	// months.
	AnchorRolling DateAnchor = "rolling"
	// AnchorCalendar is a whole calendar period: "last year" means that year
	// from 1 January to 1 January, "this month" the current month from the 1st.
	AnchorCalendar DateAnchor = "calendar"
	// AnchorAbsolute is a period named outright: "2024", "January 2024".
	AnchorAbsolute DateAnchor = "absolute"
)

// DateExpr is a recognised date expression, normalised but NOT resolved.
//
// Extraction produces a descriptor; it does not compute timestamps. The
// boundaries of "last month" depend on a clock and a timezone that belong to
// the request, and resolving here would freeze both into the layer that can
// least justify them. Resolve does the arithmetic, and Phase 6's planner is
// what calls it.
type DateExpr struct {
	Unit   DateUnit
	Anchor DateAnchor
	// Count is how many units the window covers. 1 for "last month", 6 for
	// "last 6 months".
	Count int
	// Offset positions a calendar period: 0 is the current one ("this month"),
	// -1 the previous ("last month").
	Offset int
	// Year and Month are set for AnchorAbsolute. Month is 0 when only a year
	// was named.
	Year  int
	Month time.Month

	Text       string
	Start, End int
}

// Resolve turns the descriptor into a HALF-OPEN range: start <= t < end.
//
// Half-open, not inclusive-inclusive, and this is the whole reason the type
// exists. "Orders in March" expressed with an inclusive end either includes
// midnight on 1 April or drops the last instant of 31 March, depending on the
// column's precision — a silent off-by-one in a report nobody audits. The
// production library models periods the same way, and decision 004 records the
// matching refusal of a BETWEEN operator.
//
// now and loc are supplied by the caller so that behaviour is testable against
// a frozen clock and honours the config timezone.
func (d DateExpr) Resolve(now time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)

	switch d.Anchor {
	case AnchorRolling:
		end = now
		start = addUnits(now, d.Unit, -d.Count)
		return start, end

	case AnchorCalendar:
		base := truncateTo(now, d.Unit, loc)
		start = addUnits(base, d.Unit, d.Offset)
		// A multi-unit calendar window ends at the start of the unit after the
		// last one it covers, which is what keeps it half-open.
		end = addUnits(start, d.Unit, maxCount(d.Count))
		return start, end

	case AnchorAbsolute:
		if d.Month != 0 {
			start = time.Date(d.Year, d.Month, 1, 0, 0, 0, 0, loc)
			return start, start.AddDate(0, 1, 0)
		}
		start = time.Date(d.Year, time.January, 1, 0, 0, 0, 0, loc)
		return start, start.AddDate(1, 0, 0)
	}
	return now, now
}

func maxCount(c int) int {
	if c < 1 {
		return 1
	}
	return c
}

// addUnits shifts t by n units.
//
// Calendar arithmetic, not multiplication by a fixed duration. Adding
// "30 * 24h" to cross a daylight-saving boundary lands an hour off, and a
// month is not a fixed number of hours at all.
func addUnits(t time.Time, unit DateUnit, n int) time.Time {
	switch unit {
	case UnitDay:
		return t.AddDate(0, 0, n)
	case UnitWeek:
		return t.AddDate(0, 0, 7*n)
	case UnitMonth:
		return t.AddDate(0, n, 0)
	case UnitQuarter:
		return t.AddDate(0, 3*n, 0)
	case UnitYear:
		return t.AddDate(n, 0, 0)
	}
	return t
}

// truncateTo returns the start of the calendar period containing t.
func truncateTo(t time.Time, unit DateUnit, loc *time.Location) time.Time {
	y, m, d := t.Date()
	switch unit {
	case UnitDay:
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	case UnitWeek:
		// Weeks start Monday. An arbitrary choice, but a fixed one: leaving it
		// to the locale would make "last week" mean different ranges on
		// different machines, which the determinism requirement rules out.
		offset := (int(t.Weekday()) + 6) % 7
		return time.Date(y, m, d-offset, 0, 0, 0, 0, loc)
	case UnitMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case UnitQuarter:
		firstMonth := time.Month((int(m)-1)/3*3 + 1)
		return time.Date(y, firstMonth, 1, 0, 0, 0, 0, loc)
	case UnitYear:
		return time.Date(y, time.January, 1, 0, 0, 0, 0, loc)
	}
	return t
}

var unitWords = map[string]DateUnit{
	"day": UnitDay, "days": UnitDay,
	"week": UnitWeek, "weeks": UnitWeek,
	"month": UnitMonth, "months": UnitMonth,
	"quarter": UnitQuarter, "quarters": UnitQuarter,
	"year": UnitYear, "years": UnitYear,
}

var monthWords = map[string]time.Month{
	"january": time.January, "jan": time.January,
	"february": time.February, "feb": time.February,
	"march": time.March, "mar": time.March,
	"april": time.April, "apr": time.April,
	"may":  time.May,
	"june": time.June, "jun": time.June,
	"july": time.July, "jul": time.July,
	"august": time.August, "aug": time.August,
	"september": time.September, "sep": time.September, "sept": time.September,
	"october": time.October, "oct": time.October,
	"november": time.November, "nov": time.November,
	"december": time.December, "dec": time.December,
}

// ExtractDates finds date expressions.
//
// Patterns recognised, in the order they are tried:
//
//	last|past|previous <n>? <unit>    rolling or calendar
//	this|current <unit>               calendar, current period
//	next <n>? <unit>                  calendar, forward
//	<month> <year>?                   absolute
//	<4-digit year>                    absolute
//	today | yesterday | ytd           fixed shorthands
//
// Anything else is left alone. An unrecognised date phrase becomes an ordinary
// candidate span, which is the right failure: it may still retrieve a date
// field semantically, and Phase 4 can refuse it for lack of a value.
func ExtractDates(tokens []Token, numbers []Number) []DateExpr {
	var out []DateExpr

	i := 0
	for i < len(tokens) {
		if d, next, ok := parseDateAt(tokens, numbers, i); ok {
			out = append(out, d)
			i = next
			continue
		}
		i++
	}
	return out
}

func parseDateAt(tokens []Token, numbers []Number, i int) (DateExpr, int, bool) {
	t := tokens[i]
	if t.Kind != KindWord && t.Kind != KindNumber {
		return DateExpr{}, i, false
	}

	switch t.Norm {
	case "today":
		return DateExpr{Unit: UnitDay, Anchor: AnchorCalendar, Count: 1, Text: t.Text, Start: i, End: i + 1}, i + 1, true
	case "yesterday":
		return DateExpr{Unit: UnitDay, Anchor: AnchorCalendar, Count: 1, Offset: -1, Text: t.Text, Start: i, End: i + 1}, i + 1, true

	case "last", "past", "previous":
		return parseBackwardWindow(tokens, numbers, i)

	case "this", "current":
		if unit, ok := unitAfter(tokens, i+1); ok {
			next := i + 2
			return DateExpr{
				Unit: unit, Anchor: AnchorCalendar, Count: 1, Offset: 0,
				Text: Text(tokens[i:next]), Start: i, End: next,
			}, next, true
		}

	case "next", "coming":
		if count, unit, next, ok := countAndUnit(tokens, numbers, i+1); ok {
			return DateExpr{
				Unit: unit, Anchor: AnchorCalendar, Count: count, Offset: 1,
				Text: Text(tokens[i:next]), Start: i, End: next,
			}, next, true
		}
	}

	if month, ok := monthWords[t.Norm]; ok {
		end := i + 1
		year := 0
		if end < len(tokens) && tokens[end].Kind == KindNumber {
			if y, err := strconv.Atoi(tokens[end].Norm); err == nil && isPlausibleYear(y) {
				year = y
				end++
			}
		}
		if year == 0 {
			// A month with no year is ambiguous — "in March" of which year? —
			// and guessing the current one would be a silent assumption. Left
			// unrecognised so it becomes an ordinary span and Phase 4 can ask.
			return DateExpr{}, i, false
		}
		return DateExpr{
			Unit: UnitMonth, Anchor: AnchorAbsolute, Year: year, Month: month,
			Text: Text(tokens[i:end]), Start: i, End: end,
		}, end, true
	}

	if t.Kind == KindNumber {
		if y, err := strconv.Atoi(t.Norm); err == nil && isPlausibleYear(y) {
			return DateExpr{
				Unit: UnitYear, Anchor: AnchorAbsolute, Year: y,
				Text: t.Text, Start: i, End: i + 1,
			}, i + 1, true
		}
	}
	return DateExpr{}, i, false
}

// parseBackwardWindow handles "last month", "last 6 months", "past 30 days".
//
// The distinction between rolling and calendar is not pedantry. "Last 6 months"
// means the six months up to now; "last month" means the previous calendar
// month. Collapsing them would shift every boundary by up to a month, and the
// error would be invisible in the SQL.
func parseBackwardWindow(tokens []Token, numbers []Number, i int) (DateExpr, int, bool) {
	count, unit, next, ok := countAndUnit(tokens, numbers, i+1)
	if !ok {
		return DateExpr{}, i, false
	}

	anchor := AnchorRolling
	offset := 0
	if count == 1 && !hadExplicitCount(tokens, numbers, i+1) {
		// Bare "last month" / "last year": the previous whole period.
		anchor = AnchorCalendar
		offset = -1
	}

	return DateExpr{
		Unit: unit, Anchor: anchor, Count: count, Offset: offset,
		Text: Text(tokens[i:next]), Start: i, End: next,
	}, next, true
}

// countAndUnit reads an optional count followed by a unit word, skipping the
// article in "the last six months".
func countAndUnit(tokens []Token, numbers []Number, i int) (count int, unit DateUnit, next int, ok bool) {
	if i < len(tokens) && tokens[i].Norm == "the" {
		i++
	}
	count = 1

	if n, found := numberStartingAt(numbers, i); found {
		c, whole := windowCount(n)
		if !whole {
			// "last 2.5 months", "last 0 days", "last -5 days": not a window
			// the grammar can express. Until the Phase 8 gate the count was
			// truncated (2.5 → 2) or raised to 1 (0 → 1), and the query
			// covered a period nobody asked for. Now no window forms, and the
			// words are left for binding, which refuses what it cannot place.
			return 0, "", i, false
		}
		count = c
		i = n.End
	}

	if u, found := unitAfter(tokens, i); found {
		return count, u, i + 1, true
	}
	return 0, "", next, false
}

func unitAfter(tokens []Token, i int) (DateUnit, bool) {
	if i >= len(tokens) {
		return "", false
	}
	u, ok := unitWords[tokens[i].Norm]
	return u, ok
}

func hadExplicitCount(tokens []Token, numbers []Number, i int) bool {
	if i < len(tokens) && tokens[i].Norm == "the" {
		i++
	}
	_, found := numberStartingAt(numbers, i)
	return found
}

// maxWindowCount bounds a window's count so that the calendar arithmetic in
// addUnits (7×n days, 3×n months) cannot overflow an int. Where the resulting
// period lands is the planner's check: it refuses one that leaves the years a
// query can express.
const maxWindowCount = 1<<31 - 1

// windowCount is a window's count: a whole number of at least 1, written
// plainly (no currency, no percent), and within maxWindowCount. Anything else
// is not a count of periods, and is reported as such (ok false) rather than
// rounded into one (decision 022 §2).
func windowCount(n Number) (int, bool) {
	if n.Malformed || n.Currency != "" || n.Percent || n.Value.Sign() < 1 {
		return 0, false
	}
	c, ok := n.Value.Int64()
	if !ok || c > maxWindowCount {
		return 0, false
	}
	return int(c), true
}

func numberStartingAt(numbers []Number, i int) (Number, bool) {
	for _, n := range numbers {
		if n.Start == i {
			return n, true
		}
	}
	return Number{}, false
}

// isPlausibleYear bounds what a bare four-digit number may be read as.
//
// Without a bound, "customers who spent more than 2000" would parse 2000 as a
// year and lose the filter value entirely. The range is deliberately narrow,
// and a number outside it stays a number.
func isPlausibleYear(y int) bool { return y >= 1990 && y <= 2100 }
