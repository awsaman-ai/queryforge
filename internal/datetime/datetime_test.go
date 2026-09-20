package datetime_test

import (
	"testing"
	"time"

	. "github.com/awsaman-ai/queryforge/internal/datetime"
)

// mustZone loads an IANA zone or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// utc is a terse UTC instant for the expectation tables.
func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// TestPeriodBounds pins the calendar arithmetic, happy path and the edges that
// break naive implementations: month ends (AddDate(0,-1,0) on Mar 31 lands in
// March), leap days, Monday week starts seen from a Sunday, year rollover, and
// zones whose midnight is not a UTC hour.
func TestPeriodBounds(t *testing.T) {
	ist := mustZone(t, "Asia/Kolkata")
	tue := utc(2026, 7, 28, 12, 0) // a Tuesday, midday UTC — the suite's FixedNow

	cases := []struct {
		name       string
		now        time.Time
		loc        *time.Location
		unit       string
		from, to   int
		start, end time.Time
	}{
		// --- the everyday phrases, UTC ---
		{"today", tue, time.UTC, "day", 0, 0, utc(2026, 7, 28, 0, 0), utc(2026, 7, 29, 0, 0)},
		{"yesterday", tue, time.UTC, "day", -1, -1, utc(2026, 7, 27, 0, 0), utc(2026, 7, 28, 0, 0)},
		{"tomorrow", tue, time.UTC, "day", 1, 1, utc(2026, 7, 29, 0, 0), utc(2026, 7, 30, 0, 0)},
		{"last 30 days incl today", tue, time.UTC, "day", -29, 0, utc(2026, 6, 29, 0, 0), utc(2026, 7, 29, 0, 0)},
		{"30 full days excl today", tue, time.UTC, "day", -30, -1, utc(2026, 6, 28, 0, 0), utc(2026, 7, 28, 0, 0)},
		{"this week (Tue -> Mon start)", tue, time.UTC, "week", 0, 0, utc(2026, 7, 27, 0, 0), utc(2026, 8, 3, 0, 0)},
		{"last week", tue, time.UTC, "week", -1, -1, utc(2026, 7, 20, 0, 0), utc(2026, 7, 27, 0, 0)},
		{"this month", tue, time.UTC, "month", 0, 0, utc(2026, 7, 1, 0, 0), utc(2026, 8, 1, 0, 0)},
		{"last month", tue, time.UTC, "month", -1, -1, utc(2026, 6, 1, 0, 0), utc(2026, 7, 1, 0, 0)},
		{"last 3 months incl this", tue, time.UTC, "month", -2, 0, utc(2026, 5, 1, 0, 0), utc(2026, 8, 1, 0, 0)},
		{"this year", tue, time.UTC, "year", 0, 0, utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
		{"last year", tue, time.UTC, "year", -1, -1, utc(2025, 1, 1, 0, 0), utc(2026, 1, 1, 0, 0)},
		{"previous two hours", utc(2026, 7, 28, 12, 40), time.UTC, "hour", -2, -1, utc(2026, 7, 28, 10, 0), utc(2026, 7, 28, 12, 0)},
		{"this minute", utc(2026, 7, 28, 12, 40), time.UTC, "minute", 0, 0, utc(2026, 7, 28, 12, 40), utc(2026, 7, 28, 12, 41)},

		// --- calendar edges ---
		{"last month from Mar 31", utc(2026, 3, 31, 10, 0), time.UTC, "month", -1, -1, utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)},
		{"leap day is yesterday", utc(2028, 3, 1, 10, 0), time.UTC, "day", -1, -1, utc(2028, 2, 29, 0, 0), utc(2028, 3, 1, 0, 0)},
		{"yesterday across new year", utc(2026, 1, 1, 2, 0), time.UTC, "day", -1, -1, utc(2025, 12, 31, 0, 0), utc(2026, 1, 1, 0, 0)},
		{"last month across new year", utc(2026, 1, 15, 2, 0), time.UTC, "month", -1, -1, utc(2025, 12, 1, 0, 0), utc(2026, 1, 1, 0, 0)},
		{"this week seen from Sunday", utc(2026, 8, 2, 10, 0), time.UTC, "week", 0, 0, utc(2026, 7, 27, 0, 0), utc(2026, 8, 3, 0, 0)},
		{"this week seen from Monday", utc(2026, 8, 3, 0, 0), time.UTC, "week", 0, 0, utc(2026, 8, 3, 0, 0), utc(2026, 8, 10, 0, 0)},
		{"a future span", tue, time.UTC, "day", 1, 7, utc(2026, 7, 29, 0, 0), utc(2026, 8, 5, 0, 0)},

		// --- timezone: IST is UTC+05:30, so local midnight is 18:30 UTC ---
		{"today in IST", tue, ist, "day", 0, 0, utc(2026, 7, 27, 18, 30), utc(2026, 7, 28, 18, 30)},
		{"IST day already rolled over", utc(2026, 7, 28, 20, 0), ist, "day", 0, 0, utc(2026, 7, 28, 18, 30), utc(2026, 7, 29, 18, 30)},
		{"exactly IST midnight is the new day", utc(2026, 7, 27, 18, 30), ist, "day", 0, 0, utc(2026, 7, 27, 18, 30), utc(2026, 7, 28, 18, 30)},
		{"IST hour boundary is :30 UTC", utc(2026, 7, 28, 12, 10), ist, "hour", 0, 0, utc(2026, 7, 28, 11, 30), utc(2026, 7, 28, 12, 30)},
		{"IST this month", tue, ist, "month", 0, 0, utc(2026, 6, 30, 18, 30), utc(2026, 7, 31, 18, 30)},

		// --- a nil location behaves like UTC (an unfinalized Config) ---
		{"nil location", tue, nil, "day", -1, -1, utc(2026, 7, 27, 0, 0), utc(2026, 7, 28, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := PeriodBounds(tc.now, tc.loc, tc.unit, tc.from, tc.to)
			if !start.Equal(tc.start) || !end.Equal(tc.end) {
				t.Errorf("PeriodBounds(%s, %d..%d) = [%s, %s), want [%s, %s)",
					tc.unit, tc.from, tc.to, start.Format(time.RFC3339), end.Format(time.RFC3339),
					tc.start.Format(time.RFC3339), tc.end.Format(time.RFC3339))
			}
			if start.Location() != time.UTC || end.Location() != time.UTC {
				t.Errorf("bounds must be returned in UTC, got %s / %s", start.Location(), end.Location())
			}
		})
	}
}

// TestPeriodBoundsAcrossDST checks that day periods stay on LOCAL midnight when
// a daylight-saving change falls inside them. The day the clocks change is 23
// or 25 hours long; a 24h-step implementation would cut it at 23:00 or 01:00.
// 2026 in America/New_York: spring forward Sun Mar 8, fall back Sun Nov 1.
func TestPeriodBoundsAcrossDST(t *testing.T) {
	ny := mustZone(t, "America/New_York")

	// Asked Mon Mar 9, 11:00 EDT. Yesterday began at 00:00 EST (05:00Z) and ended
	// at 00:00 EDT (04:00Z): 23 hours.
	start, end := PeriodBounds(utc(2026, 3, 9, 15, 0), ny, "day", -1, -1)
	if !start.Equal(utc(2026, 3, 8, 5, 0)) || !end.Equal(utc(2026, 3, 9, 4, 0)) {
		t.Errorf("spring-forward yesterday = [%s, %s)", start, end)
	}
	if got := end.Sub(start); got != 23*time.Hour {
		t.Errorf("spring-forward day length = %v, want 23h", got)
	}

	// Asked Mon Nov 2, 10:00 EST. Yesterday began at 00:00 EDT (04:00Z) and ended
	// at 00:00 EST (05:00Z): 25 hours.
	start, end = PeriodBounds(utc(2026, 11, 2, 15, 0), ny, "day", -1, -1)
	if !start.Equal(utc(2026, 11, 1, 4, 0)) || !end.Equal(utc(2026, 11, 2, 5, 0)) {
		t.Errorf("fall-back yesterday = [%s, %s)", start, end)
	}
	if got := end.Sub(start); got != 25*time.Hour {
		t.Errorf("fall-back day length = %v, want 25h", got)
	}

	// Hours shift on the absolute clock: "the previous 3 hours" is 3 real hours
	// even when the wall clock jumped from 02:00 to 03:00 inside them.
	start, end = PeriodBounds(utc(2026, 3, 8, 8, 30), ny, "hour", -3, -1) // 04:30 EDT
	if got := end.Sub(start); got != 3*time.Hour {
		t.Errorf("hour span across spring-forward = %v, want 3h (start %s, end %s)", got, start, end)
	}
}

// TestPeriodBoundsIgnoreTimeOfDay is the property that motivated periods: the
// same question asked at any moment of the same local day selects the same
// range. A rolling relative_date fails this by construction.
func TestPeriodBoundsIgnoreTimeOfDay(t *testing.T) {
	ist := mustZone(t, "Asia/Kolkata")
	for _, loc := range []*time.Location{time.UTC, ist} {
		dayStart := time.Date(2026, 7, 28, 0, 0, 0, 0, loc)
		wantS, wantE := PeriodBounds(dayStart, loc, "day", -29, 0)
		for _, offset := range []time.Duration{time.Nanosecond, 9 * time.Hour, 23*time.Hour + 59*time.Minute + 59*time.Second + 999*time.Millisecond} {
			s, e := PeriodBounds(dayStart.Add(offset), loc, "day", -29, 0)
			if !s.Equal(wantS) || !e.Equal(wantE) {
				t.Errorf("%s +%v: [%s, %s) differs from the start of the day's [%s, %s)", loc, offset, s, e, wantS, wantE)
			}
		}
	}
}

// TestPeriodBoundsExtremeOffsets checks the validator's maximum magnitude
// resolves without overflow or a reversed range, for every unit.
func TestPeriodBoundsExtremeOffsets(t *testing.T) {
	now := utc(2026, 7, 28, 12, 0)
	for _, unit := range RelativeUnits {
		start, end := PeriodBounds(now, time.UTC, unit, -MaxRelativeAmount, MaxRelativeAmount)
		if !start.Before(now) || !end.After(now) {
			t.Errorf("%s ±%d: range [%s, %s) does not contain now — an offset overflowed",
				unit, MaxRelativeAmount, start, end)
		}
	}
}
