package queryforge

import (
	"fmt"
	"strconv"
	"time"
)

// Elasticsearch/OpenSearch source (index) resolution.
//
// Security boundary this file exists to hold: source/index configuration is
// trusted, the query is untrusted input (see the package README's design
// rationale, and Config.Scope for the same principle applied to filters).
// Every index name a resolution can produce — Default, a rule's or branch's
// Indexes, or IndexPattern's literal text — was written into the config by
// whoever owns it. The query only ever supplies the VALUE of a field the
// config explicitly opted into routing (Field.RoutingField, checked at load
// by validateIndexRouting), and even that value is never written into an
// index name without a final validESIndexName check. There is no path from
// "the model wrote an unexpected condition" to "QueryForge searched an index
// the config never named".
//
// Resolution only ever trusts a predicate that must hold for every row the
// query can return — one reachable from the filter tree's root through AND
// alone. A predicate inside an OR branch, or inside a NOT, does not
// necessarily constrain the result set, so it is never used to pick an index;
// see collectRoutingConditions.

// resolveESIndex determines the physical index/indexes an Elasticsearch/
// OpenSearch query should target, given the backend's configured source mode.
// It is a pure function of (q, c, now) — no network, no hidden state — same
// as everything else a Generator does.
func resolveESIndex(q *Query, c *Config, backend string, now time.Time) ([]string, SourceType, error) {
	bc, ok := c.Backends[backend]
	if !ok {
		return []string{q.Entity}, SourceDirectIndex, nil
	}
	switch {
	case bc.Index != "":
		return []string{bc.Index}, SourceDirectIndex, nil
	case bc.Alias != "":
		return []string{bc.Alias}, SourceAlias, nil
	case len(bc.Indexes) > 0:
		return append([]string(nil), bc.Indexes...), SourceMultipleIndex, nil
	case bc.Routing != nil:
		indexes, err := resolveBusinessRuleIndex(q, c, bc.Routing, now)
		if err != nil {
			return nil, "", err
		}
		return indexes, SourceBusinessRule, nil
	default:
		return []string{q.Entity}, SourceDirectIndex, nil
	}
}

// resolveBusinessRuleIndex dispatches to the configured routing strategy.
func resolveBusinessRuleIndex(q *Query, c *Config, r *IndexRouting, now time.Time) ([]string, error) {
	switch r.Strategy {
	case RoutingPattern:
		return resolvePatternRouting(q, r)
	case RoutingDate:
		return resolveDateRouting(q, r, now)
	case RoutingRules:
		return resolveRulesRouting(q, c, r, now)
	case RoutingIfElse:
		return resolveIfElseRouting(q, c, r, now)
	default:
		return nil, fmt.Errorf("elasticsearch: unknown routing strategy %q", r.Strategy)
	}
}

// collectRoutingConditions gathers every comparison node on fieldName that is
// reachable from cond through AND alone, appending to out. It deliberately
// does not descend into OR or NOT: a predicate there is not guaranteed to
// hold for every row the query returns, so it is not a sound basis for
// choosing which index to search.
func collectRoutingConditions(cond *Condition, fieldName string, out *[]*Condition) {
	if cond == nil {
		return
	}
	switch cond.Type {
	case CondComparison:
		if cond.Field == fieldName {
			*out = append(*out, cond)
		}
	case CondLogical:
		if cond.Op == OpAND {
			for _, ch := range cond.Children {
				collectRoutingConditions(ch, fieldName, out)
			}
		}
		// OR and NOT: not descended, per the doc comment above.
	}
}

// singleRoutingValue returns the one AND-reachable comparison on fieldName
// usable for routing — an equals/gt/gte/lt/lte/before/after node with a
// scalar (non-array, non-relative) value — or ok=false when there is none, or
// more than one (ambiguous: which value would even apply?), or its operator
// carries no single comparable literal.
func singleRoutingValue(q *Query, fieldName string) (op Operator, val *Value, ok bool) {
	var matches []*Condition
	collectRoutingConditions(q.Filter, fieldName, &matches)
	if len(matches) != 1 {
		return "", nil, false
	}
	cond := matches[0]
	switch cond.Operator {
	case OpEquals, OpGt, OpGte, OpLt, OpLte, OpBefore, OpAfter:
	default:
		return "", nil, false
	}
	if cond.Value == nil || cond.Value.Kind == KindArray {
		return "", nil, false
	}
	return cond.Operator, cond.Value, true
}

// betweenRoutingValue returns the single AND-reachable "between" comparison
// on fieldName, when there is exactly one and only one such node.
func betweenRoutingValue(q *Query, fieldName string) (lo, hi string, ok bool) {
	var matches []*Condition
	collectRoutingConditions(q.Filter, fieldName, &matches)
	var between *Condition
	for _, m := range matches {
		if m.Operator == OpBetween {
			if between != nil {
				return "", "", false // more than one: ambiguous
			}
			between = m
		}
	}
	if between == nil || between.Value == nil {
		return "", "", false
	}
	elems, ok := between.Value.AsSlice()
	if !ok || len(elems) != 2 {
		return "", "", false
	}
	loS, ok1 := elems[0].(string)
	hiS, ok2 := elems[1].(string)
	if !ok1 || !ok2 {
		return "", "", false
	}
	return loS, hiS, true
}

// resolvePatternRouting substitutes the routing field's literal value into
// IndexPattern's token, falling back to Default when the field is absent or
// its value cannot be read as one of a single, comparable literal.
func resolvePatternRouting(q *Query, r *IndexRouting) ([]string, error) {
	_, val, ok := singleRoutingValue(q, r.Field)
	if !ok {
		return r.Default, nil
	}
	lit, ok := patternLiteral(val)
	if !ok {
		return r.Default, nil
	}
	token, _ := extractPatternToken(r.IndexPattern) // already validated at load
	name := substituteToken(r.IndexPattern, token, lit)
	if !validESIndexName(name) {
		return nil, fmt.Errorf("elasticsearch: business-rule index %q resolved from field %q's value is not a valid index name",
			name, r.Field)
	}
	return []string{name}, nil
}

// patternLiteral renders a Value's payload as the literal text a Pattern
// routing substitution inserts into an index name.
func patternLiteral(v *Value) (string, bool) {
	switch v.Kind {
	case KindString, KindEnum:
		s, ok := v.AsString()
		return s, ok
	case KindNumber:
		f, ok := v.AsFloat()
		if !ok {
			return "", false
		}
		return strconv.FormatFloat(f, 'f', -1, 64), true
	default:
		return "", false // dates go through resolveDateRouting instead
	}
}

// substituteToken replaces pattern's "{token}" placeholder with value.
func substituteToken(pattern, token, value string) string {
	out := make([]byte, 0, len(pattern)+len(value))
	placeholder := "{" + token + "}"
	idx := indexOf(pattern, placeholder)
	out = append(out, pattern[:idx]...)
	out = append(out, value...)
	out = append(out, pattern[idx+len(placeholder):]...)
	return string(out)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// resolveDateRouting resolves the physical index(es) a date-partitioned
// routing field selects.
//
// Only two shapes are supported, both bounded by construction: a single
// equals (one instant, one partition) and a single between (an inclusive
// range, expanded to every partition it spans). An unbounded before/after —
// "orders before 2020" — is deliberately NOT resolved to a partition list: a
// finite bound the config never promised could span the entire index set, and
// silently expanding a request into "search everything" is exactly the
// unrestricted-wildcard behaviour the package guarantees against. It falls
// back to Default instead, same as an absent field.
func resolveDateRouting(q *Query, r *IndexRouting, now time.Time) ([]string, error) {
	if lo, hi, ok := betweenRoutingValue(q, r.Field); ok {
		start, err := parseRoutingDate(lo)
		if err != nil {
			return r.Default, nil //nolint:nilerr // unparsable literal: treat as unresolvable, not fatal
		}
		end, err := parseRoutingDate(hi)
		if err != nil {
			return r.Default, nil //nolint:nilerr
		}
		return expandDatePartitions(start, end, r.Granularity, r.IndexPattern)
	}

	op, val, ok := singleRoutingValue(q, r.Field)
	if !ok || op != OpEquals {
		return r.Default, nil
	}
	t, err := resolveDateValue(val, now)
	if err != nil {
		return r.Default, nil //nolint:nilerr
	}
	return expandDatePartitions(t, t, r.Granularity, r.IndexPattern)
}

// parseRoutingDate parses a between-bound's literal string as an absolute
// date.
func parseRoutingDate(s string) (time.Time, error) {
	if t, ok := parseDate(s).(time.Time); ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("elasticsearch: %q is not a recognised date", s)
}

// resolveDateValue reads a Value as an absolute date, resolving a relative
// date against now the same way every other generator does.
func resolveDateValue(v *Value, now time.Time) (time.Time, error) {
	if v.Kind == KindRelativeDate {
		return resolveRelative(now, v.Unit, v.Amount)
	}
	s, ok := v.AsString()
	if !ok {
		return time.Time{}, fmt.Errorf("elasticsearch: date routing value is not a string")
	}
	t, ok := parseDate(s).(time.Time)
	if !ok {
		return time.Time{}, fmt.Errorf("elasticsearch: %q is not a recognised date", s)
	}
	return t, nil
}

// expandDatePartitions returns every partition label between start and end
// inclusive, at the given granularity, each substituted into pattern and
// validated as a safe index name.
func expandDatePartitions(start, end time.Time, g DateGranularity, pattern string) ([]string, error) {
	if end.Before(start) {
		start, end = end, start
	}
	token, _ := extractPatternToken(pattern) // already validated at load
	layout, step := granularityLayout(g)

	var out []string
	seen := make(map[string]bool)
	for t := truncateTo(start, g); !t.After(end); t = step(t) {
		name := substituteToken(pattern, token, t.Format(layout))
		if !validESIndexName(name) {
			return nil, fmt.Errorf("elasticsearch: business-rule index %q resolved from a date partition is not a valid index name", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("elasticsearch: date routing resolved no partitions for range %s to %s", start, end)
	}
	return out, nil
}

// granularityLayout returns the Go time layout for one partition label and a
// step function that advances to the next partition boundary.
func granularityLayout(g DateGranularity) (layout string, step func(time.Time) time.Time) {
	switch g {
	case GranularityMonth:
		return "2006-01", func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
	case GranularityDay:
		return "2006-01-02", func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	default: // YEAR
		return "2006", func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }
	}
}

// truncateTo rounds t down to the start of its partition.
func truncateTo(t time.Time, g DateGranularity) time.Time {
	switch g {
	case GranularityMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	case GranularityDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	default: // YEAR
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
	}
}

// resolveRulesRouting evaluates every rule's condition against the query,
// returning the Indexes of the highest-priority match. Zero matches falls
// back to Default; two or more matches tied at the top priority is a
// resolution error rather than a guess.
func resolveRulesRouting(q *Query, c *Config, r *IndexRouting, now time.Time) ([]string, error) {
	type hit struct {
		indexes  []string
		priority int
		i        int
	}
	var hits []hit
	for i, rule := range r.Rules {
		ok, err := evalRoutingCondition(q, c, rule.When, now)
		if err != nil {
			return nil, err
		}
		if ok {
			hits = append(hits, hit{rule.Indexes, rule.Priority, i})
		}
	}
	if len(hits) == 0 {
		return r.Default, nil
	}
	best := hits[0]
	tied := false
	for _, h := range hits[1:] {
		switch {
		case h.priority > best.priority:
			best, tied = h, false
		case h.priority == best.priority:
			tied = true
		}
	}
	if tied {
		return nil, fmt.Errorf("elasticsearch: business-rule routing is ambiguous — more than one rule matches "+
			"this query at priority %d; add distinct priorities to disambiguate", best.priority)
	}
	return best.indexes, nil
}

// resolveIfElseRouting evaluates branches in order, returning the first
// match's Indexes (an Else:true branch always matches). No match falls back
// to Default.
func resolveIfElseRouting(q *Query, c *Config, r *IndexRouting, now time.Time) ([]string, error) {
	for _, br := range r.Branches {
		if br.Else {
			return br.Indexes, nil
		}
		ok, err := evalRoutingCondition(q, c, *br.If, now)
		if err != nil {
			return nil, err
		}
		if ok {
			return br.Indexes, nil
		}
	}
	return r.Default, nil
}

// evalRoutingCondition reports whether the query's known value for
// cond.Field satisfies cond. A field with no single AND-reachable comparable
// value (absent, inside an OR/NOT, or multi-valued) simply does not match —
// routing never guesses.
func evalRoutingCondition(q *Query, c *Config, cond RoutingCondition, now time.Time) (bool, error) {
	_, val, ok := singleRoutingValue(q, cond.Field)
	if !ok {
		return false, nil
	}
	f, _ := c.FieldByName(cond.Field)
	switch f.Type {
	case FieldNumber:
		got, ok := val.AsFloat()
		if !ok {
			return false, nil
		}
		want, err := strconv.ParseFloat(cond.Value, 64)
		if err != nil {
			return false, fmt.Errorf("elasticsearch: routing condition on %q: %w", cond.Field, err)
		}
		return compareFloat(got, cond.Operator, want), nil
	case FieldDate:
		got, err := resolveDateValue(val, now)
		if err != nil {
			return false, nil //nolint:nilerr
		}
		want, ok := parseDate(cond.Value).(time.Time)
		if !ok {
			return false, fmt.Errorf("elasticsearch: routing condition on %q: %q is not a recognised date", cond.Field, cond.Value)
		}
		return compareTime(got, cond.Operator, want), nil
	default: // string, enum
		got, ok := val.AsString()
		if !ok {
			return false, nil
		}
		switch cond.Operator {
		case OpEquals:
			return got == cond.Value, nil
		case OpNotEquals:
			return got != cond.Value, nil
		default:
			return false, nil
		}
	}
}

func compareFloat(got float64, op Operator, want float64) bool {
	switch op {
	case OpEquals:
		return got == want
	case OpNotEquals:
		return got != want
	case OpGt:
		return got > want
	case OpGte:
		return got >= want
	case OpLt:
		return got < want
	case OpLte:
		return got <= want
	default:
		return false
	}
}

func compareTime(got time.Time, op Operator, want time.Time) bool {
	switch op {
	case OpEquals:
		return got.Equal(want)
	case OpNotEquals:
		return !got.Equal(want)
	case OpGt:
		return got.After(want)
	case OpGte:
		return !got.Before(want)
	case OpLt:
		return got.Before(want)
	case OpLte:
		return !got.After(want)
	default:
		return false
	}
}
