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

	// Projection clause.
	if len(q.Select) > 0 {
		sb.WriteString("Return ")
		sb.WriteString(strings.Join(q.Select, ", "))
	} else {
		sb.WriteString("Return all fields")
	}
	sb.WriteString(" from ")
	sb.WriteString(q.Entity)

	// Filter clause.
	if q.Filter != nil {
		sb.WriteString(" where ")
		sb.WriteString(describeCondition(q.Filter))
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
		sb.WriteString(fmt.Sprintf(", limited to %d result(s)", *q.Limit))
	}
	if q.Offset != nil && *q.Offset > 0 {
		sb.WriteString(fmt.Sprintf(", skipping the first %d", *q.Offset))
	}

	sb.WriteString(".")
	return sb.String()
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
	plural := unit
	if n != 1 {
		plural = unit + "s" // naive pluralization is fine for day/week/month/year/hour/minute
	}
	return fmt.Sprintf("%d %s %s", n, plural, suffix)
}
