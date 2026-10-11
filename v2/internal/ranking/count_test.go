package ranking

import "testing"

// Decision 022 §2: "a count sits next to this phrase" is evidence only for a
// positive whole number a LIMIT can hold (int64). Exact numbers must not turn
// a literal the float64 test rejected into a count: before the gate a value
// past int64 failed the float test, and it must still fail it now that it
// arrives exactly (found by mutation: dropping the range check changed no
// other test).
func TestCountEvidenceNeedsAnInt64Count(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	top := []hit{{"pattern:top_n", 0.825, "top N"}, {"pattern:bottom_n", 0.4, "bottom N"}}
	for _, c := range []struct {
		n     string
		count bool
	}{
		{"5", true},
		{"9223372036854775807", true},
		{"9223372036854775808", false},
		{"99999999999999999999999", false},
		{"2.5", false},
		{"-5", false},
	} {
		res := mustRank(t, script(t, reg, "top "+c.n+" customers", map[string][]hit{"top": top}), DefaultPolicy())
		s := spanNamed(t, res, "top")
		var ctx float64
		for _, cand := range s.Candidates {
			if cand.Entry.ID == "pattern:top_n" {
				ctx = cand.Evidence.Context
			}
		}
		if (ctx == 1) != c.count {
			t.Errorf("top %s: count evidence %v, want %v", c.n, ctx == 1, c.count)
		}
	}
}
