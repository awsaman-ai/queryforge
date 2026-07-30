package queryforge

import (
	"fmt"
	"strings"
	"time"
)

// SQLGenerator compiles an AST into parameterized SQL (Postgres dialect). Every
// value the user could influence is bound as a placeholder argument ($1, $2, …)
// rather than interpolated, so the generated SQL is injection-safe. Only
// physical identifiers (table/column names), which come from the trusted config
// and never from user text, are written into the statement directly. The output
// is always a read: a single SELECT, never a mutation.
type SQLGenerator struct{}

// Backend returns the registry id for this generator.
func (SQLGenerator) Backend() string { return "sql" }

// Generate builds the SELECT statement and its bound arguments.
func (g SQLGenerator) Generate(q *Query, c *Config, opts GenOptions) (*Result, error) {
	b := &sqlBuilder{now: opts.now()} // accumulates placeholder args in order

	var sb strings.Builder // the statement text
	sb.WriteString("SELECT ")

	// Projection: explicit select list (mapped to physical columns) or "*".
	if fields, explicit := projectionFields(q, c); explicit {
		// An empty allow-list would render "SELECT  FROM t" — invalid SQL. It
		// means the config marks every field non-returnable, so there is no
		// query to build; say so rather than emitting a broken statement.
		if len(fields) == 0 {
			return nil, fmt.Errorf("generate sql: no returnable fields: the config marks every field returnable:false, so the projection would be empty")
		}
		cols := make([]string, len(fields))
		for i, f := range fields {
			cols[i] = c.PhysicalName(f, "sql")
		}
		sb.WriteString(strings.Join(cols, ", "))
	} else {
		sb.WriteString("*")
	}

	sb.WriteString(" FROM ")
	sb.WriteString(sqlTable(q, c)) // physical table name

	// WHERE clause, if the AST carries a filter.
	if q.Filter != nil {
		where, err := b.condition(c, q.Filter)
		if err != nil {
			return nil, err
		}
		if where != "" {
			sb.WriteString(" WHERE ")
			sb.WriteString(where)
		}
	}

	// ORDER BY, mapping each logical field to its physical column.
	if len(q.Sort) > 0 {
		parts := make([]string, len(q.Sort))
		for i, s := range q.Sort {
			dir := strings.ToUpper(s.Dir) // normalize case
			if dir == "" {
				dir = "ASC" // default ordering when unspecified
			}
			parts[i] = c.PhysicalName(s.Field, "sql") + " " + dir
		}
		sb.WriteString(" ORDER BY ")
		sb.WriteString(strings.Join(parts, ", "))
	}

	// LIMIT / OFFSET. These are integers from the AST/config (never user
	// strings), so inlining them is safe and readable.
	if lim := effectiveLimit(q, c); lim > 0 {
		fmt.Fprintf(&sb, " LIMIT %d", lim)
	}
	if off := effectiveOffset(q); off > 0 {
		fmt.Fprintf(&sb, " OFFSET %d", off)
	}

	return &Result{
		Backend:  "sql",
		SQL:      sb.String(),
		Args:     b.args,
		Warnings: collectIndexWarnings(q, c),
	}, nil
}

// sqlTable resolves the physical table name from the backend mapping, falling
// back to the entity name when no mapping is configured.
func sqlTable(q *Query, c *Config) string {
	if bc, ok := c.Backends["sql"]; ok && bc.Source() != "" {
		return bc.Source()
	}
	return q.Entity
}

// sqlBuilder threads the growing argument list (and reference time) through the
// recursive condition rendering.
type sqlBuilder struct {
	args []any     // bound arguments, positionally matching $1, $2, …
	now  time.Time // reference time for relative dates
}

// bind appends an argument and returns its placeholder token ($N).
func (b *sqlBuilder) bind(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// condition dispatches on node type.
func (b *sqlBuilder) condition(c *Config, cond *Condition) (string, error) {
	switch cond.Type {
	case CondLogical:
		return b.logical(c, cond)
	case CondComparison:
		return b.comparison(c, cond)
	default:
		return "", fmt.Errorf("sql: unknown condition type %q", cond.Type)
	}
}

// logical renders AND/OR/NOT, reordering children by index/priority first.
func (b *sqlBuilder) logical(c *Config, cond *Condition) (string, error) {
	children := orderedChildren(c, cond.Children) // predicate ordering
	parts := make([]string, 0, len(children))
	for _, ch := range children {
		s, err := b.condition(c, ch)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	switch cond.Op {
	case OpAND:
		return "(" + strings.Join(parts, " AND ") + ")", nil
	case OpOR:
		return "(" + strings.Join(parts, " OR ") + ")", nil
	case OpNOT:
		return "NOT (" + parts[0] + ")", nil // validator guarantees exactly one child
	default:
		return "", fmt.Errorf("sql: unknown logical operator %q", cond.Op)
	}
}

// comparison renders a single predicate. Date semantics follow the design doc:
// after/before are inclusive (>= / <=).
func (b *sqlBuilder) comparison(c *Config, cond *Condition) (string, error) {
	field := c.PhysicalName(cond.Field, "sql") // logical -> physical column

	switch cond.Operator {
	case OpIsNull:
		return field + " IS NULL", nil
	case OpIsNotNull:
		return field + " IS NOT NULL", nil
	case OpEquals:
		return b.scalar(field, "=", cond.Value)
	case OpNotEquals:
		return b.scalar(field, "<>", cond.Value)
	case OpGt:
		return b.scalar(field, ">", cond.Value)
	case OpLt:
		return b.scalar(field, "<", cond.Value)
	case OpGte:
		return b.scalar(field, ">=", cond.Value)
	case OpLte:
		return b.scalar(field, "<=", cond.Value)
	case OpAfter:
		return b.scalar(field, ">=", cond.Value)
	case OpBefore:
		return b.scalar(field, "<=", cond.Value)
	case OpRegex:
		return b.scalar(field, "~", cond.Value)
	case OpBetween:
		return b.between(field, cond.Value)
	case OpIn:
		return b.inList(field, "IN", cond.Value)
	case OpNotIn:
		return b.inList(field, "NOT IN", cond.Value)
	case OpStartsWith:
		return b.like(field, "", "%", cond.Value) // value%
	case OpEndsWith:
		return b.like(field, "%", "", cond.Value) // %value
	case OpContains:
		return b.contains(c, field, cond)
	case OpContainsAny:
		return b.arrayOp(field, "&&", cond.Value) // array overlap
	case OpContainsAll:
		return b.arrayOp(field, "@>", cond.Value) // array contains
	default:
		return "", fmt.Errorf("sql: unsupported operator %q", cond.Operator)
	}
}

// scalar renders "field <op> $N" for a single bound value.
func (b *sqlBuilder) scalar(field, sqlOp string, v *Value) (string, error) {
	arg, err := b.value(v)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s %s", field, sqlOp, b.bind(arg)), nil
}

// between renders "field BETWEEN $a AND $b" from a 2-element array value.
func (b *sqlBuilder) between(field string, v *Value) (string, error) {
	elems, _ := v.AsSlice() // validator guaranteed a 2-element array
	return fmt.Sprintf("%s BETWEEN %s AND %s", field, b.bind(elems[0]), b.bind(elems[1])), nil
}

// inList renders "field IN ($1, $2, …)" (or NOT IN) from an array value.
func (b *sqlBuilder) inList(field, keyword string, v *Value) (string, error) {
	elems, _ := v.AsSlice()
	phs := make([]string, len(elems))
	for i, e := range elems {
		phs[i] = b.bind(e)
	}
	return fmt.Sprintf("%s %s (%s)", field, keyword, strings.Join(phs, ", ")), nil
}

// arrayOp renders a Postgres array operator against an ARRAY[...] literal of
// bound placeholders, e.g. "tags @> ARRAY[$1, $2]".
func (b *sqlBuilder) arrayOp(field, sqlOp string, v *Value) (string, error) {
	elems, _ := v.AsSlice()
	phs := make([]string, len(elems))
	for i, e := range elems {
		phs[i] = b.bind(e)
	}
	return fmt.Sprintf("%s %s ARRAY[%s]", field, sqlOp, strings.Join(phs, ", ")), nil
}

// like renders a LIKE predicate, wrapping the bound value with the given
// prefix/suffix wildcards.
func (b *sqlBuilder) like(field, pre, post string, v *Value) (string, error) {
	s, ok := v.AsString()
	if !ok {
		return "", fmt.Errorf("sql: LIKE requires a string value on %s", field)
	}
	return fmt.Sprintf("%s LIKE %s", field, b.bind(pre+s+post)), nil
}

// contains behaves differently by field type: array membership uses the
// Postgres @> operator; a string "contains" is a %substring% LIKE.
func (b *sqlBuilder) contains(c *Config, field string, cond *Condition) (string, error) {
	if f, ok := c.FieldByName(cond.Field); ok && f.Type == FieldArray {
		arg, err := b.value(cond.Value)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s @> ARRAY[%s]", field, b.bind(arg)), nil
	}
	return b.like(field, "%", "%", cond.Value) // %value% substring match
}

// value converts a scalar AST Value into the Go argument bound to a placeholder.
func (b *sqlBuilder) value(v *Value) (any, error) {
	switch v.Kind {
	case KindString, KindEnum, KindDate:
		s, _ := v.AsString()
		return s, nil
	case KindNumber:
		f, _ := v.AsFloat()
		return f, nil
	case KindBoolean:
		bb, _ := v.AsBool()
		return bb, nil
	case KindRelativeDate:
		return resolveRelative(b.now, v.Unit, v.Amount), nil // absolute time, bound as a param
	default:
		return nil, fmt.Errorf("sql: cannot bind value of kind %q", v.Kind)
	}
}
