package queryforge

import (
	"fmt"
	"strings"
)

// Explain renders a validated AST as human-readable prose. It is fully
// deterministic — a plain rendering of the tree, with no model call and no
// execution — so it is safe to show a user before they run anything (the
// "dry run" / explain-before-execute safeguard). It never resolves relative
// dates against a clock; it describes them symbolically ("30 days ago") so the
// output is stable.
func Explain(q *Query, c *Config) string {
	if q == nil {
		return "(empty query)"
	}

	var sb strings.Builder

	// Separate the caller's scope from the user's own conditions, so the prose
	// reads as "here is what you asked for" followed by "and here is what was
	// applied regardless". Merged into one sentence, a forced tenant predicate
	// would look like something the question requested — the opposite of what an
	// explain-before-execute readback is for. Both are nil when no scope was
	// injected, leaving the wording of every existing explanation untouched.
	scoped, userFilter := splitScope(q.Filter)

	// Projection clause.
	if len(q.Select) > 0 {
		sb.WriteString("Return ")
		sb.WriteString(strings.Join(q.Select, ", "))
	} else {
		sb.WriteString("Return all fields")
	}
	sb.WriteString(" from ")
	sb.WriteString(q.Entity)

	// Filter clause — the user's conditions only; scope is reported below.
	if userFilter != nil {
		sb.WriteString(" where ")
		sb.WriteString(describeCondition(userFilter))
	}

	// Sort clause.
	if len(q.Sort) > 0 {
		parts := make([]string, len(q.Sort))
		for i, s := range q.Sort {
			dir := "ascending"
			if strings.EqualFold(s.Dir, "DESC") {
				dir = "descending"
			}
			parts[i] = fmt.Sprintf("%s (%s)", s.Field, dir)
		}
		sb.WriteString(", sorted by ")
		sb.WriteString(strings.Join(parts, ", "))
	}

	// Paging clause.
	if q.Limit != nil {
		fmt.Fprintf(&sb, ", limited to %d result(s)", *q.Limit)
	}
	if q.Offset != nil && *q.Offset > 0 {
		fmt.Fprintf(&sb, ", skipping the first %d", *q.Offset)
	}

	sb.WriteString(".")

	// Scope clause. "Always" is the operative word: these predicates hold for
	// every query this caller makes, whatever the question was.
	if len(scoped) > 0 {
		parts := make([]string, len(scoped))
		for i, cond := range scoped {
			parts[i] = describeComparison(cond)
		}
		sb.WriteString(" Always scoped to ")
		sb.WriteString(strings.Join(parts, ", "))
		sb.WriteString(".")
	}

	// Same-element clause. When two predicates land on one array of
	// sub-documents, "sku ABC and price over 100" has two readings — one item
	// that is both, or two items that are each one — and the prose above cannot
	// tell them apart. The Mongo compilation picks the same-element reading, so
	// an explain-before-execute readback has to say which one it picked.
	for _, path := range sameElementArrays(q.Filter, c) {
		fmt.Fprintf(&sb, " Conditions on %s apply to the same array element.", path)
	}
	return sb.String()
}

// sameElementArrays returns, in stable order, the arrays of sub-documents that
// carry more than one ANDed predicate — exactly the groups the Mongo generator
// folds into a single $elemMatch. Only AND is considered: an OR branch is
// satisfied independently, so nothing is being grouped there.
func sameElementArrays(cond *Condition, c *Config) []string {
	if cond == nil || c == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{} // report each array once, however deep it recurs

	var walk func(*Condition)
	walk = func(n *Condition) {
		if n == nil || n.Type != CondLogical {
			return
		}
		if n.Op == OpAND {
			counts := map[string]int{} // array path -> ANDed predicates on it
			var order []string         // first-seen order, so output is stable
			for _, ch := range n.Children {
				if ch == nil || ch.Type != CondComparison {
					continue
				}
				if path, _, ok := c.MongoElemMatch(ch.Field); ok {
					if counts[path] == 0 {
						order = append(order, path)
					}
					counts[path]++
				}
			}
			for _, path := range order {
				if counts[path] > 1 && !seen[path] {
					seen[path] = true
					out = append(out, path)
				}
			}
		}
		for _, ch := range n.Children { // grouping can occur in any nested AND
			walk(ch)
		}
	}
	walk(cond)
	return out
}

// describeCondition renders one node of the filter tree as prose, parenthesizing
// nested groups so precedence is unambiguous.
func describeCondition(cond *Condition) string {
	if cond == nil {
		return "(nil)"
	}
	switch cond.Type {
	case CondComparison:
		return describeComparison(cond)
	case CondLogical:
		return describeLogical(cond)
	default:
		return fmt.Sprintf("(unknown condition %q)", cond.Type)
	}
}

// describeLogical joins children with AND/OR or negates a single child.
func describeLogical(cond *Condition) string {
	parts := make([]string, len(cond.Children)) // one phrase per child
	for i, ch := range cond.Children {
		parts[i] = describeCondition(ch)
	}
	switch cond.Op {
	case OpAND:
		return "(" + strings.Join(parts, " AND ") + ")"
	case OpOR:
		return "(" + strings.Join(parts, " OR ") + ")"
	case OpNOT:
		if len(parts) == 1 {
			return "NOT " + parts[0]
		}
		return "NOT (" + strings.Join(parts, ", ") + ")"
	default:
		return "(" + strings.Join(parts, " ? ") + ")"
	}
}

// describeComparison renders "field <phrase> value".
func describeComparison(cond *Condition) string {
	phrase := operatorPhrase(cond.Operator) // English for the operator
	if isNullOperator(cond.Operator) {      // null operators take no value
		return cond.Field + " " + phrase
	}
	return cond.Field + " " + phrase + " " + describeValue(cond.Operator, cond.Value)
}

// operatorPhrase maps an operator to a readable phrase.
func operatorPhrase(op Operator) string {
	switch op {
	case OpEquals:
		return "equals"
	case OpNotEquals:
		return "does not equal"
	case OpGt:
		return "is greater than"
	case OpLt:
		return "is less than"
	case OpGte:
		return "is at least"
	case OpLte:
		return "is at most"
	case OpBetween:
		return "is between"
	case OpIn:
		return "is one of"
	case OpNotIn:
		return "is not one of"
	case OpContains:
		return "contains"
	case OpContainsAny:
		return "contains any of"
	case OpContainsAll:
		return "contains all of"
	case OpStartsWith:
		return "starts with"
	case OpEndsWith:
		return "ends with"
	case OpRegex:
		return "matches"
	case OpBefore:
		return "is on or before"
	case OpAfter:
		return "is on or after"
	case OpIsNull:
		return "is empty"
	case OpIsNotNull:
		return "is not empty"
	default:
		return string(op)
	}
}

// describeValue renders a Value for prose, handling the array-valued operators
// (between joins with "and", the rest list their members).
func describeValue(op Operator, v *Value) string {
	if v == nil {
		return "(no value)"
	}
	if v.Kind == KindArray {
		elems, _ := v.AsSlice()
		strs := make([]string, len(elems))
		for i, e := range elems {
			strs[i] = fmt.Sprintf("%v", e)
		}
		if op == OpBetween && len(strs) == 2 {
			return strs[0] + " and " + strs[1]
		}
		return "[" + strings.Join(strs, ", ") + "]"
	}
	return describeScalar(v)
}

// describeScalar renders a single Value, giving relative dates a friendly form.
func describeScalar(v *Value) string {
	switch v.Kind {
	case KindRelativeDate:
		return describeRelative(v.Unit, v.Amount)
	case KindString, KindEnum, KindDate:
		s, _ := v.AsString()
		return fmt.Sprintf("%q", s) // quote text so boundaries are clear
	case KindNumber:
		f, _ := v.AsFloat()
		return fmt.Sprintf("%v", f)
	case KindBoolean:
		b, _ := v.AsBool()
		return fmt.Sprintf("%v", b)
	default:
		return fmt.Sprintf("%v", v.V)
	}
}

// describeRelative turns (unit, amount) into "30 days ago" / "7 days from now".
func describeRelative(unit string, amount int) string {
	n := amount
	suffix := "from now"
	if amount < 0 {
		n = -amount
		suffix = "ago"
	}
	return fmt.Sprintf("%d %s %s", n, pluralizeUnit(unit, n), suffix)
}

// pluralizeUnit renders a relative-date unit for n of them.
//
// Appending "s" to whatever arrived was fine for the six known units and wrong
// for everything else: an AST carrying "days" rendered "30 dayss ago", and an
// empty unit rendered "30  ago". That mattered more than a typo, because the
// explanation is the readback a user is shown BEFORE running the query — the one
// place a malformed unit was visible at all, and the doubled letter was subtle
// enough that no operator would catch it. The validator now rejects such units
// outright, so this is the second line of defence: an unrecognised unit is
// rendered loudly rather than plausibly.
func pluralizeUnit(unit string, n int) string {
	if !isRelativeUnit(unit) {
		return fmt.Sprintf("<invalid unit %q>", unit)
	}
	if n == 1 {
		return unit
	}
	return unit + "s"
}
