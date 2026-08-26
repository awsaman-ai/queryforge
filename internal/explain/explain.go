// Package explain renders a validated AST as deterministic prose — the
// dry-run readback a user can be shown before anything runs.
//
// splitScope lives here rather than in internal/scope, its only caller's former
// neighbour, because scope needs this package's DescribeComparison to render a
// ScopeFilter for a log line. One of the two had to move, and splitScope is the
// half that exists purely to shape an explanation.
package explain

import (
	"fmt"
	"strings"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
	"github.com/awsaman-ai/queryforge/internal/datetime"
)

// Explain renders a validated AST as human-readable prose. It is fully
// deterministic — a plain rendering of the tree, with no model call and no
// execution — so it is safe to show a user before they run anything (the
// "dry run" / explain-before-execute safeguard). It never resolves relative
// dates against a clock; it describes them symbolically ("30 days ago") so the
// output is stable.
func Explain(q *ast.Query, c *config.Config) string {
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
		sb.WriteString(describeCondition(userFilter, c))
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
			parts[i] = DescribeComparison(cond, c)
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
func sameElementArrays(cond *ast.Condition, c *config.Config) []string {
	if cond == nil || c == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{} // report each array once, however deep it recurs

	var walk func(*ast.Condition)
	walk = func(n *ast.Condition) {
		if n == nil || n.Type != ast.CondLogical {
			return
		}
		if n.Op == ast.OpAND {
			counts := map[string]int{} // array path -> ANDed predicates on it
			var order []string         // first-seen order, so output is stable
			for _, ch := range n.Children {
				if ch == nil || ch.Type != ast.CondComparison {
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
// nested groups so precedence is unambiguous. c is threaded through purely to
// resolve a field's DisplayName; it may be nil, in which case every field
// falls back to its raw AST name.
func describeCondition(cond *ast.Condition, c *config.Config) string {
	if cond == nil {
		return "(nil)"
	}
	switch cond.Type {
	case ast.CondComparison:
		return DescribeComparison(cond, c)
	case ast.CondLogical:
		return describeLogical(cond, c)
	default:
		return fmt.Sprintf("(unknown condition %q)", cond.Type)
	}
}

// describeLogical joins children with AND/OR or negates a single child.
func describeLogical(cond *ast.Condition, c *config.Config) string {
	parts := make([]string, len(cond.Children)) // one phrase per child
	for i, ch := range cond.Children {
		parts[i] = describeCondition(ch, c)
	}
	switch cond.Op {
	case ast.OpAND:
		return "(" + strings.Join(parts, " AND ") + ")"
	case ast.OpOR:
		return "(" + strings.Join(parts, " OR ") + ")"
	case ast.OpNOT:
		if len(parts) == 1 {
			return "NOT " + parts[0]
		}
		return "NOT (" + strings.Join(parts, ", ") + ")"
	default:
		return "(" + strings.Join(parts, " ? ") + ")"
	}
}

// DescribeComparison renders "field <phrase> value", using the field's
// configured DisplayName in place of its raw AST name when one is set.
func DescribeComparison(cond *ast.Condition, c *config.Config) string {
	label := fieldLabel(cond.Field, c)
	phrase := operatorPhrase(cond.Operator) // English for the operator
	if ast.IsNullOperator(cond.Operator) {  // null operators take no value
		return label + " " + phrase
	}
	return label + " " + phrase + " " + describeValue(cond.Operator, cond.Value)
}

// fieldLabel returns the field's configured DisplayName for prose, falling
// back to the raw AST field name when the field isn't registered (e.g. an
// unmapped scope key — see Scope's doc comment) or sets no label.
func fieldLabel(name string, c *config.Config) string {
	if c == nil {
		return name
	}
	if f, ok := c.FieldByName(name); ok && f.DisplayName != "" {
		return f.DisplayName
	}
	return name
}

// operatorPhrase maps an operator to a readable phrase.
func operatorPhrase(op ast.Operator) string {
	switch op {
	case ast.OpEquals:
		return "equals"
	case ast.OpNotEquals:
		return "does not equal"
	case ast.OpGt:
		return "is greater than"
	case ast.OpLt:
		return "is less than"
	case ast.OpGte:
		return "is at least"
	case ast.OpLte:
		return "is at most"
	case ast.OpBetween:
		return "is between"
	case ast.OpIn:
		return "is one of"
	case ast.OpNotIn:
		return "is not one of"
	case ast.OpContains:
		return "contains"
	case ast.OpContainsAny:
		return "contains any of"
	case ast.OpContainsAll:
		return "contains all of"
	case ast.OpStartsWith:
		return "starts with"
	case ast.OpEndsWith:
		return "ends with"
	case ast.OpRegex:
		return "matches"
	case ast.OpBefore:
		return "is on or before"
	case ast.OpAfter:
		return "is on or after"
	case ast.OpIsNull:
		return "is empty"
	case ast.OpIsNotNull:
		return "is not empty"
	default:
		return string(op)
	}
}

// describeValue renders a Value for prose, handling the array-valued operators
// (between joins with "and", the rest list their members).
func describeValue(op ast.Operator, v *ast.Value) string {
	if v == nil {
		return "(no value)"
	}
	if v.Kind == ast.KindArray {
		elems, _ := v.AsSlice()
		strs := make([]string, len(elems))
		for i, e := range elems {
			strs[i] = fmt.Sprintf("%v", e)
		}
		if op == ast.OpBetween && len(strs) == 2 {
			return strs[0] + " and " + strs[1]
		}
		return "[" + strings.Join(strs, ", ") + "]"
	}
	return describeScalar(v)
}

// describeScalar renders a single Value, giving relative dates a friendly form.
func describeScalar(v *ast.Value) string {
	switch v.Kind {
	case ast.KindRelativeDate:
		return DescribeRelative(v.Unit, v.Amount)
	case ast.KindString, ast.KindEnum, ast.KindDate:
		s, _ := v.AsString()
		return fmt.Sprintf("%q", s) // quote text so boundaries are clear
	case ast.KindNumber:
		f, _ := v.AsFloat()
		return fmt.Sprintf("%v", f)
	case ast.KindBoolean:
		b, _ := v.AsBool()
		return fmt.Sprintf("%v", b)
	default:
		return fmt.Sprintf("%v", v.V)
	}
}

// DescribeRelative turns (unit, amount) into "30 days ago" / "7 days from now".
func DescribeRelative(unit string, amount int) string {
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
	if !datetime.IsRelativeUnit(unit) {
		return fmt.Sprintf("<invalid unit %q>", unit)
	}
	if n == 1 {
		return unit
	}
	return unit + "s"
}

// splitScope separates the caller-injected predicates from the user-intent
// filter, so an explanation can present them as what they are — a fixed scope
// the question did not choose — instead of burying them among the user's own
// conditions. Returns (nil, cond) unchanged when no scope was injected.
func splitScope(cond *ast.Condition) (scoped []*ast.Condition, rest *ast.Condition) {
	if cond == nil {
		return nil, nil
	}
	if cond.Scoped { // a single scope predicate and no user filter
		return []*ast.Condition{cond}, nil
	}
	if cond.Type != ast.CondLogical || cond.Op != ast.OpAND {
		return nil, cond // scope is only ever spliced into a root AND
	}

	others := make([]*ast.Condition, 0, len(cond.Children))
	for _, ch := range cond.Children {
		if ch != nil && ch.Scoped {
			scoped = append(scoped, ch)
			continue
		}
		others = append(others, ch)
	}
	if len(scoped) == 0 {
		return nil, cond // ordinary AND: leave it exactly as it was
	}

	switch len(others) {
	case 0:
		return scoped, nil // scope only
	case 1:
		return scoped, others[0] // drop the now-redundant AND wrapper
	default:
		return scoped, &ast.Condition{Type: ast.CondLogical, Op: ast.OpAND, Children: others}
	}
}
