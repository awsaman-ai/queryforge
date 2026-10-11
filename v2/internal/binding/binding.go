// Package binding combines the independent readings ranking accepted into one
// coherent meaning: which entity the sentence is about, which conditions apply
// to it, through which joins, and what is to be counted, aggregated or ranked.
//
// Section 13 of the brief. The example it gives is the whole job in miniature:
//
//	customers   → customer entity
//	spent       → total_spent field          ⇒  Condition
//	more than   → GT operator                     field:    customer.total_spent
//	50000       → numeric value                   operator: gt
//	                                              value:    50000
//
// Binding is deterministic structural reasoning over what ranking decided. It
// never re-ranks, never re-embeds, never consults similarity. Every rule is a
// statement about the order of readings in the sentence and about the schema,
// and every refusal names the rule that fired. The output is NOT the
// SemanticPlan (that is Phase 6's planner input); it is the checked meaning
// the planner will be built from.
//
// REDACTION (decisions 006, 013): stored values (alias targets, enum values)
// and literals from the sentence are carried as qferr.Sensitive, so formatting
// a Binding by accident redacts them. Structure — entities, fields, operators,
// relationships, paths — is always shown.
package binding

import (
	"fmt"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/numeric"
	"queryforge.local/v2experiment/internal/qferr"
)

// FieldRef names a configured field.
type FieldRef struct {
	Entity string
	Field  string
	Type   config.FieldType
}

func (f FieldRef) String() string { return f.Entity + "." + f.Field }

// ValueKind says where a condition's value came from.
type ValueKind string

const (
	// ValueNumber is a number the sentence wrote.
	ValueNumber ValueKind = "number"
	// ValueDate is a date expression. It stays a descriptor here; resolving it
	// against a clock and timezone is the planner's job (Phase 3's decision).
	ValueDate ValueKind = "date"
	// ValueAlias is a stored value reached through a configured value alias.
	ValueAlias ValueKind = "alias"
	// ValueEnum is a stored enum value the sentence named directly.
	ValueEnum ValueKind = "enum"
	// ValueLiteral is free text from the sentence, compared to a string field.
	ValueLiteral ValueKind = "literal"
)

// Value is a condition's right-hand side.
type Value struct {
	Kind ValueKind
	// Number is the number exactly as the sentence wrote it (decision 022 §2).
	Number numeric.Decimal
	// Currency and Percent are carried, not acted on (Phase 3).
	Currency string
	Percent  bool
	Date     *nlp.DateExpr
	// Stored is the value to compare against, for alias, enum and literal
	// kinds. Sensitive: it is data, not structure.
	Stored qferr.Sensitive
	// Unit is a word written after a number ("3 times"), kept for the
	// explanation and not interpreted. Sensitive: it is the user's text.
	Unit qferr.Sensitive
}

// String renders the value with data redacted.
func (v Value) String() string { return v.render(false) }

// Render renders the value, revealing stored values and literals only when
// reveal is set. The explicit form for output code; String is the safe
// default for accidents.
func (v Value) Render(reveal bool) string { return v.render(reveal) }

func (v Value) render(reveal bool) string {
	switch v.Kind {
	case ValueNumber:
		s := v.Number.String()
		if v.Currency != "" {
			s += " " + v.Currency
		}
		if v.Percent {
			s += "%"
		}
		return s
	case ValueDate:
		if v.Date == nil {
			return "date(?)"
		}
		d := v.Date
		switch d.Anchor {
		case nlp.AnchorAbsolute:
			if d.Month != 0 {
				return fmt.Sprintf("date(%04d-%02d)", d.Year, int(d.Month))
			}
			return fmt.Sprintf("date(%04d)", d.Year)
		default:
			return fmt.Sprintf("date(%s %s, count %d, offset %d)", d.Anchor, d.Unit, d.Count, d.Offset)
		}
	default:
		if reveal {
			return fmt.Sprintf("%q (%s)", v.Stored.Reveal(), v.Kind)
		}
		return fmt.Sprintf("%s (%s)", qferr.Placeholder, v.Kind)
	}
}

// ConditionKind distinguishes the shapes a condition can take.
type ConditionKind string

const (
	// CondCompare is field <op> value.
	CondCompare ConditionKind = "compare"
	// CondWindow is a date field restricted to a date expression's range,
	// half-open when resolved (Phase 3).
	CondWindow ConditionKind = "window"
	// CondCount compares the number of related rows through a relationship:
	// "customers who ordered more than 3 times".
	CondCount ConditionKind = "count"
	// CondExists requires at least one related row: "customers who ordered".
	CondExists ConditionKind = "exists"
)

// Condition is one restriction on the root entity.
type Condition struct {
	Kind ConditionKind
	// Field is the compared field for compare and window.
	Field FieldRef
	// Relationship is what count and exists traverse.
	Relationship string
	Op           config.Operator
	Value        Value
	// Negated records that "not" applied; Op is already complemented.
	Negated bool
	// Inferred records that the field was chosen by the schema, not named in
	// the sentence ("orders last month" → the one date field of order).
	Inferred bool
	// Path joins the root to the entity this condition is about. Empty when
	// the condition is on the root itself.
	Path graph.Path
	// Join is how this condition combines with the previous one: "and" or
	// "or". Empty for the first.
	Join string
	// Start and End are the token range the condition was read from.
	Start, End int
}

// Aggregate is a function over a field, or count(*) of an entity.
type Aggregate struct {
	Func config.Aggregation
	// Field is nil for count(*).
	Field *FieldRef
	// Entity is what is counted when Field is nil.
	Entity string
	Path   graph.Path
}

// Pattern is a structural template the sentence invoked.
type Pattern struct {
	Name   string
	Intent config.PatternIntent
	// N is the row count for top_n. The DIRECTION (highest vs lowest) is not
	// bound here: PatternIntent does not carry one, and the TODO already
	// schedules revisiting PatternIntent against a real planner in Phase 6.
	N      int
	Metric *FieldRef
	Path   graph.Path
}

// Selection is a field the sentence names without constraining it — a column
// the answer should show.
type Selection struct {
	Field FieldRef
	Path  graph.Path
}

// Binding is the bound meaning of one sentence.
type Binding struct {
	Root       string
	Conditions []Condition
	Aggregates []Aggregate
	Pattern    *Pattern
	Selections []Selection
	// Joins lists every distinct path from the root the binding needs,
	// ordered by target entity.
	Joins []graph.Path
	// Notes are structural observations that are not errors: a relationship
	// that is trivially satisfied, a unit word kept but not interpreted. No
	// user text and no values.
	Notes []string
}

// Explain renders the binding for a person: structure always, values
// redacted unless reveal.
func (b Binding) Explain(reveal bool) string {
	var s strings.Builder
	fmt.Fprintf(&s, "root: %s\n", b.Root)
	if len(b.Joins) > 0 {
		s.WriteString("joins:\n")
		for _, p := range b.Joins {
			fmt.Fprintf(&s, "  %s\n", p)
			for _, st := range p.Steps {
				many := "to-one"
				if st.ToMany {
					many = "to-many"
				}
				fmt.Fprintf(&s, "      %s.%s = %s.%s  (%s, %s)\n",
					st.From.Entity, st.From.Field, st.To.Entity, st.To.Field, st.Relationship, many)
			}
		}
	}
	if len(b.Conditions) > 0 {
		s.WriteString("conditions:\n")
		for _, c := range b.Conditions {
			join := "    "
			if c.Join != "" {
				join = fmt.Sprintf("%-4s", strings.ToUpper(c.Join))
			}
			fmt.Fprintf(&s, "  %s %s\n", join, c.describe(reveal))
		}
	}
	for _, a := range b.Aggregates {
		target := a.Entity + ".*"
		if a.Field != nil {
			target = a.Field.String()
		}
		fmt.Fprintf(&s, "aggregate: %s(%s)", a.Func, target)
		if a.Path.Len() > 0 {
			fmt.Fprintf(&s, "  via %s", a.Path)
		}
		s.WriteString("\n")
	}
	if p := b.Pattern; p != nil {
		fmt.Fprintf(&s, "pattern: %s (intent %s)", p.Name, p.Intent)
		if p.N > 0 {
			fmt.Fprintf(&s, ", N=%d", p.N)
		}
		if p.Metric != nil {
			fmt.Fprintf(&s, ", metric %s", p.Metric)
		}
		s.WriteString("\n")
	}
	for _, sel := range b.Selections {
		fmt.Fprintf(&s, "select: %s\n", sel.Field)
	}
	for _, n := range b.Notes {
		fmt.Fprintf(&s, "note: %s\n", n)
	}
	return s.String()
}

func (c Condition) describe(reveal bool) string {
	not := ""
	if c.Negated {
		not = " (negated)"
	}
	inferred := ""
	if c.Inferred {
		inferred = " (field inferred from the schema)"
	}
	via := ""
	if c.Path.Len() > 0 {
		via = "  via " + c.Path.String()
	}
	switch c.Kind {
	case CondWindow:
		return fmt.Sprintf("%s within %s%s%s%s", c.Field, c.Value.render(reveal), not, inferred, via)
	case CondCount:
		return fmt.Sprintf("count(%s) %s %s%s%s", c.Relationship, c.Op, c.Value.render(reveal), not, via)
	case CondExists:
		return fmt.Sprintf("exists(%s)%s%s", c.Relationship, not, via)
	default:
		return fmt.Sprintf("%s %s %s%s%s%s", c.Field, c.Op, c.Value.render(reveal), not, inferred, via)
	}
}

// String is the redacted explanation, so %v of a Binding cannot leak.
func (b Binding) String() string { return b.Explain(false) }

// GoString covers %#v.
func (b Binding) GoString() string { return "binding.Binding{" + b.Explain(false) + "}" }
