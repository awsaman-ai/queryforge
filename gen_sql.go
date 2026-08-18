package queryforge

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// sqlDialect is everything that differs between two SQL backends. Factoring it
// out is what lets one compiler serve Postgres and MySQL: the AST walk, the
// value binding, the projection and paging rules are identical, and only the
// four things below are not.
//
// Adding a third dialect means adding a value of this type and registering a
// generator around it — no second copy of the walk to keep in step.
type sqlDialect struct {
	backend     string      // registry id, e.g. "sql", "mysql"
	quoter      identQuoter // identifier delimiters
	placeholder func(n int) string
	regexOp     string // infix regex-match operator

	// arrayOverlap and arrayContains render the two array predicates against a
	// list of already-bound placeholders. Postgres has native array operators;
	// MySQL has JSON functions with the arguments the other way round, so this
	// has to be a function rather than an operator token.
	arrayOverlap  func(field string, phs []string) string
	arrayContains func(field string, phs []string) string

	// bindArrayWhole is true for a dialect that binds an array predicate's
	// operand as ONE argument (a JSON document) rather than as one placeholder
	// per element. MySQL's JSON_CONTAINS/JSON_OVERLAPS take a JSON array, so the
	// elements are marshalled together.
	bindArrayWhole bool
}

// postgresDialect is the original dialect, unchanged in what it emits.
var postgresDialect = sqlDialect{
	backend:     "sql",
	quoter:      identQuoter{open: `"`, close: `"`},
	placeholder: func(n int) string { return fmt.Sprintf("$%d", n) },
	regexOp:     "~",
	arrayOverlap: func(field string, phs []string) string {
		return fmt.Sprintf("%s && ARRAY[%s]", field, strings.Join(phs, ", "))
	},
	arrayContains: func(field string, phs []string) string {
		return fmt.Sprintf("%s @> ARRAY[%s]", field, strings.Join(phs, ", "))
	},
}

// mysqlDialect targets MySQL 8.0+ (and MariaDB 10.5+ for everything except the
// JSON array predicates).
//
// It exists because the library shipped a single "sql" backend whose output is
// Postgres-only — `$1` placeholders, `~`, `ARRAY[…]`, `@>` — while the README
// says "SQL". A reader who connected MySQL got a parse error on their very first
// query, before any of the library's guarantees had a chance to matter.
//
// The array operators map onto MySQL's JSON functions, which is the only
// portable answer: MySQL has no array type, so a config's array field is a JSON
// column. JSON_OVERLAPS is "any of these" and JSON_CONTAINS is "all of these",
// matching Postgres's && and @> respectively.
var mysqlDialect = sqlDialect{
	backend:     "mysql",
	quoter:      identQuoter{open: "`", close: "`"},
	placeholder: func(int) string { return "?" },
	regexOp:     "REGEXP",
	arrayOverlap: func(field string, phs []string) string {
		return fmt.Sprintf("JSON_OVERLAPS(%s, CAST(%s AS JSON))", field, phs[0])
	},
	arrayContains: func(field string, phs []string) string {
		return fmt.Sprintf("JSON_CONTAINS(%s, CAST(%s AS JSON))", field, phs[0])
	},
	bindArrayWhole: true,
}

// SQLGenerator compiles an AST into parameterized SQL (Postgres dialect). Every
// value the user could influence is bound as a placeholder argument ($1, $2, …)
// rather than interpolated, so the generated SQL is injection-safe. Only
// physical identifiers (table/column names) are written into the statement
// directly; those are checked against the identifier rule first (identifier.go)
// and quoted when the dialect requires it. The output is always a read: a single
// SELECT, never a mutation.
type SQLGenerator struct{}

// Backend returns the registry id for this generator.
func (SQLGenerator) Backend() string { return "sql" }

// Generate builds the Postgres SELECT statement and its bound arguments.
func (SQLGenerator) Generate(q *Query, c *Config, opts GenOptions) (*Result, error) {
	return generateSQL(postgresDialect, q, c, opts)
}

// MySQLGenerator compiles an AST into parameterized MySQL. It is the same
// compiler as SQLGenerator with the MySQL dialect: `?` placeholders, backtick
// quoting, REGEXP, and JSON_CONTAINS/JSON_OVERLAPS for array predicates.
//
// Register it explicitly (DefaultRegistry does) and ask for backend "mysql";
// "sql" remains Postgres, so no existing caller changes meaning.
type MySQLGenerator struct{}

// Backend returns the registry id for this generator.
func (MySQLGenerator) Backend() string { return "mysql" }

// Generate builds the MySQL SELECT statement and its bound arguments.
func (MySQLGenerator) Generate(q *Query, c *Config, opts GenOptions) (*Result, error) {
	return generateSQL(mysqlDialect, q, c, opts)
}

// isSQLBackend reports whether a backend id names one of the SQL dialects, whose
// physical names are written into statement text and are therefore held to the
// identifier rule at config load.
func isSQLBackend(backend string) bool {
	return backend == postgresDialect.backend || backend == mysqlDialect.backend
}

// generateSQL is the dialect-independent compiler.
func generateSQL(d sqlDialect, q *Query, c *Config, opts GenOptions) (*Result, error) {
	b := &sqlBuilder{dialect: d, now: opts.now()} // accumulates placeholder args in order

	var sb strings.Builder // the statement text
	sb.WriteString("SELECT ")

	// Projection: explicit select list (mapped to physical columns) or "*".
	if fields, explicit := projectionFields(q, c); explicit {
		// An empty allow-list would render "SELECT  FROM t" — invalid SQL. It
		// means the config marks every field non-returnable, so there is no
		// query to build; say so rather than emitting a broken statement.
		if len(fields) == 0 {
			return nil, fmt.Errorf("generate %s: no returnable fields: the config marks every field returnable:false, so the projection would be empty", d.backend)
		}
		cols := make([]string, len(fields))
		for i, f := range fields {
			col, err := b.ident(sqlPhysicalName(c, f, d.backend))
			if err != nil {
				return nil, err
			}
			cols[i] = col
		}
		sb.WriteString(strings.Join(cols, ", "))
	} else {
		sb.WriteString("*")
	}

	table, err := b.ident(sqlTable(q, c, d.backend)) // physical table name
	if err != nil {
		return nil, err
	}
	sb.WriteString(" FROM ")
	sb.WriteString(table)

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
			col, err := b.ident(sqlPhysicalName(c, s.Field, d.backend))
			if err != nil {
				return nil, err
			}
			parts[i] = col + " " + dir
		}
		sb.WriteString(" ORDER BY ")
		sb.WriteString(strings.Join(parts, ", "))
	}

	// LIMIT / OFFSET. These are integers from the AST/config (never user
	// strings), so inlining them is safe and readable.
	//
	// MySQL cannot parse OFFSET without a preceding LIMIT, so an offset-only
	// query gets the dialect's "no ceiling" limit rather than a statement the
	// server rejects. Postgres accepts either alone.
	lim := effectiveLimit(q, c)
	off := effectiveOffset(q)
	switch {
	case lim > 0:
		fmt.Fprintf(&sb, " LIMIT %d", lim)
	case off > 0 && d.backend == mysqlDialect.backend:
		sb.WriteString(mysqlNoLimitClause)
	}
	if off > 0 {
		fmt.Fprintf(&sb, " OFFSET %d", off)
	}

	return &Result{
		Backend:  d.backend,
		SQL:      sb.String(),
		Args:     b.args,
		Warnings: collectIndexWarnings(q, c),
	}, nil
}

// mysqlNoLimitClause is what MySQL's own documentation prescribes for
// "everything from this offset on": OFFSET is not parseable without a LIMIT, so
// the row count is written as the maximum BIGINT. It is a literal string rather
// than an int because that value does not fit in a Go int.
const mysqlNoLimitClause = " LIMIT 18446744073709551615"

// sqlTable resolves the physical table name from the backend mapping, falling
// back to the entity name when no mapping is configured.
//
// A config that maps only "sql" is still the right table for "mysql" — the
// entity lives in one place whichever driver reaches it — so the Postgres
// mapping is the fallback for every other SQL dialect before the entity name is.
func sqlTable(q *Query, c *Config, backend string) string {
	if bc, ok := c.Backends[backend]; ok && bc.Source() != "" {
		return bc.Source()
	}
	if backend != postgresDialect.backend {
		if bc, ok := c.Backends[postgresDialect.backend]; ok && bc.Source() != "" {
			return bc.Source()
		}
	}
	return q.Entity
}

// sqlPhysicalName resolves a field's physical column for a SQL dialect, with the
// same "sql" fallback sqlTable uses: a config written before the MySQL backend
// existed maps `customerName -> customer_name` under "sql", and that is the
// column name on MySQL too. An explicit "mysql" mapping still wins, for the
// config that genuinely needs two different names.
func sqlPhysicalName(c *Config, field, backend string) string {
	name := c.PhysicalName(field, backend)
	if name != field || backend == postgresDialect.backend {
		return name // an explicit mapping for this dialect, or nothing to fall back to
	}
	return c.PhysicalName(field, postgresDialect.backend)
}

// sqlBuilder threads the growing argument list (and reference time) through the
// recursive condition rendering.
type sqlBuilder struct {
	dialect sqlDialect // placeholder/quoting/operator spellings
	args    []any      // bound arguments, positionally matching the placeholders
	now     time.Time  // reference time for relative dates
}

// bind appends an argument and returns its placeholder token.
func (b *sqlBuilder) bind(v any) string {
	b.args = append(b.args, v)
	return b.dialect.placeholder(len(b.args))
}

// ident checks a physical name and renders it for the dialect.
//
// The check is the point: an identifier cannot be bound, so it is the one part
// of the statement built from a string rather than from an argument. Config load
// already rejects a bad SQL mapping, but a Config built in Go, a Mongo-oriented
// field name, or a scope key on an undeclared field all reach here without
// having passed that gate, and a name that is not an identifier must become an
// error rather than syntax.
func (b *sqlBuilder) ident(name string) (string, error) {
	if !validIdentPath(name) {
		return "", fmt.Errorf("%s: %q is not a valid identifier: a physical column or table name must be "+
			"dot-separated letters, digits and underscores, not starting with a digit", b.dialect.backend, name)
	}
	return quoteIdentPath(name, b.dialect.quoter), nil
}

// condition dispatches on node type.
func (b *sqlBuilder) condition(c *Config, cond *Condition) (string, error) {
	switch cond.Type {
	case CondLogical:
		return b.logical(c, cond)
	case CondComparison:
		return b.comparison(c, cond)
	default:
		return "", fmt.Errorf("%s: unknown condition type %q", b.dialect.backend, cond.Type)
	}
}

// logical renders AND/OR/NOT, reordering children by index/priority first.
func (b *sqlBuilder) logical(c *Config, cond *Condition) (string, error) {
	// Arity is checked here rather than assumed. The validator does guarantee it
	// on the Engine path, but Generator is an exported interface and
	// DefaultRegistry an exported constructor, so a caller fanning out to several
	// backends straight from the registry reaches this code with whatever AST it
	// holds. "NOT with no children" was an index-out-of-range panic — a 500 and a
	// dropped connection in a service — where every other bad node here is an
	// error.
	if len(cond.Children) == 0 {
		return "", fmt.Errorf("%s: logical %q has no children", b.dialect.backend, cond.Op)
	}
	if cond.Op == OpNOT && len(cond.Children) != 1 {
		return "", fmt.Errorf("%s: NOT must have exactly one child, got %d", b.dialect.backend, len(cond.Children))
	}

	children := orderedChildren(c, cond.Children) // predicate ordering
	parts := make([]string, 0, len(children))
	for _, ch := range children {
		if ch == nil {
			return "", fmt.Errorf("%s: logical %q has a nil child", b.dialect.backend, cond.Op)
		}
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
		return "NOT (" + parts[0] + ")", nil
	default:
		return "", fmt.Errorf("%s: unknown logical operator %q", b.dialect.backend, cond.Op)
	}
}

// comparison renders a single predicate. Date semantics follow the design doc:
// after/before are inclusive (>= / <=).
//
// The field's declared value-case rule is resolved once here and threaded into
// every value-rendering helper, so a single lookup covers scalars, list
// elements and LIKE patterns alike. OpRegex deliberately passes CaseAsIs: the
// value there is a pattern, and recasing it would rewrite escapes (\d means
// "digit", \D means "not a digit") into a different query.
func (b *sqlBuilder) comparison(c *Config, cond *Condition) (string, error) {
	field, err := b.ident(sqlPhysicalName(c, cond.Field, b.dialect.backend)) // logical -> physical column
	if err != nil {
		return "", err
	}
	vc := c.valueCaseFor(cond.Field) // "" unless the config asked for a case
	dt := condScalarType(c, cond)    // the declared type every literal is read as

	// caseInsensitive only ever means something for a plain string field — load
	// rejects it on any other type — but this path is also reachable straight
	// through the exported Generator with a hand-built AST that never saw
	// validation, so the type is re-checked here rather than trusted.
	ci := dt == FieldString && c.caseInsensitiveFor(cond.Field)

	switch cond.Operator {
	case OpIsNull:
		return field + " IS NULL", nil
	case OpIsNotNull:
		return field + " IS NOT NULL", nil
	case OpEquals:
		return b.scalar(field, "=", cond.Value, dt, vc, ci)
	case OpNotEquals:
		return b.scalar(field, "<>", cond.Value, dt, vc, ci)
	case OpGt:
		return b.scalar(field, ">", cond.Value, dt, vc, false)
	case OpLt:
		return b.scalar(field, "<", cond.Value, dt, vc, false)
	case OpGte:
		return b.scalar(field, ">=", cond.Value, dt, vc, false)
	case OpLte:
		return b.scalar(field, "<=", cond.Value, dt, vc, false)
	case OpAfter:
		return b.scalar(field, ">=", cond.Value, dt, vc, false)
	case OpBefore:
		return b.scalar(field, "<=", cond.Value, dt, vc, false)
	case OpRegex:
		// A pattern is text whatever the column holds, and is never recased —
		// folding it through caseInsensitive would rewrite \d into \D just as
		// surely as valueCase would.
		return b.scalar(field, b.dialect.regexOp, cond.Value, FieldString, CaseAsIs, false)
	case OpBetween:
		return b.between(field, cond.Value, vc)
	case OpIn:
		return b.inList(field, "IN", OpIn, cond.Value, vc, ci)
	case OpNotIn:
		return b.inList(field, "NOT IN", OpNotIn, cond.Value, vc, ci)
	case OpStartsWith:
		return b.like(field, "", "%", cond.Value, vc, ci) // value%
	case OpEndsWith:
		return b.like(field, "%", "", cond.Value, vc, ci) // %value
	case OpContains:
		return b.contains(c, field, cond, vc, ci)
	case OpContainsAny:
		return b.arrayOp(field, OpContainsAny, b.dialect.arrayOverlap, cond.Value, vc)
	case OpContainsAll:
		return b.arrayOp(field, OpContainsAll, b.dialect.arrayContains, cond.Value, vc)
	default:
		return "", fmt.Errorf("%s: unsupported operator %q", b.dialect.backend, cond.Operator)
	}
}

// scalar renders "field <op> $N" for a single bound value. When ci is set,
// both sides are folded through LOWER() so the comparison is blind to case —
// the column reference in the rendered text, the bound argument at bind time.
func (b *sqlBuilder) scalar(field, sqlOp string, v *Value, declared FieldType, vc ValueCase, ci bool) (string, error) {
	arg, err := b.value(v, declared, vc)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s %s", foldColumn(field, ci), sqlOp, b.bind(foldArg(arg, ci))), nil
}

// foldColumn wraps a column reference in LOWER(...) for a case-insensitive
// comparison, so both sides of the predicate compare the same folded form.
func foldColumn(field string, ci bool) string {
	if !ci {
		return field
	}
	return "LOWER(" + field + ")"
}

// foldArg lowers a bound string argument to match a LOWER(column) comparison.
// Every caller only ever sets ci for a FieldString predicate, so v is always a
// string in practice; the type assertion is a defensive no-op otherwise,
// leaving a non-string argument untouched rather than panicking.
func foldArg(v any, ci bool) any {
	if !ci {
		return v
	}
	if s, ok := v.(string); ok {
		return strings.ToLower(s)
	}
	return v
}

// sliceValue reads an array-valued operand, requiring the arity the operator
// needs. On the Engine path the validator has already guaranteed both, but this
// is an exported code path in its own right (see logical), and reading elems[0]
// on a scalar value was a panic.
func sliceValue(backend string, op Operator, v *Value, want int) ([]any, error) {
	if v == nil {
		return nil, fmt.Errorf("%s: operator %q requires a value", backend, op)
	}
	elems, ok := v.AsSlice()
	if !ok {
		return nil, fmt.Errorf("%s: operator %q requires an array value", backend, op)
	}
	if want > 0 && len(elems) != want {
		return nil, fmt.Errorf("%s: operator %q requires exactly %d values, got %d", backend, op, want, len(elems))
	}
	if want == 0 && len(elems) == 0 {
		return nil, fmt.Errorf("%s: operator %q requires at least one value", backend, op)
	}
	return elems, nil
}

// between renders "field BETWEEN $a AND $b" from a 2-element array value.
func (b *sqlBuilder) between(field string, v *Value, vc ValueCase) (string, error) {
	elems, err := sliceValue(b.dialect.backend, OpBetween, v, 2)
	if err != nil {
		return "", err
	}
	elems = vc.applyElems(elems)
	return fmt.Sprintf("%s BETWEEN %s AND %s", field, b.bind(elems[0]), b.bind(elems[1])), nil
}

// inList renders "field IN ($1, $2, …)" (or NOT IN) from an array value.
func (b *sqlBuilder) inList(field, keyword string, op Operator, v *Value, vc ValueCase, ci bool) (string, error) {
	elems, err := sliceValue(b.dialect.backend, op, v, 0)
	if err != nil {
		return "", err
	}
	elems = vc.applyElems(elems)
	phs := make([]string, len(elems))
	for i, e := range elems {
		phs[i] = b.bind(foldArg(e, ci))
	}
	return fmt.Sprintf("%s %s (%s)", foldColumn(field, ci), keyword, strings.Join(phs, ", ")), nil
}

// arrayOp renders the dialect's array predicate against the bound elements —
// "tags @> ARRAY[$1, $2]" on Postgres, "JSON_CONTAINS(tags, CAST(? AS JSON))"
// on MySQL.
func (b *sqlBuilder) arrayOp(field string, op Operator, render func(string, []string) string, v *Value, vc ValueCase) (string, error) {
	elems, err := sliceValue(b.dialect.backend, op, v, 0)
	if err != nil {
		return "", err
	}
	elems = vc.applyElems(elems)

	if b.dialect.bindArrayWhole {
		// One argument, the whole list as a JSON document. Marshalling can only
		// fail on a value JSON cannot express, which the validator's element
		// typing already excludes; report it rather than binding "null".
		doc, err := json.Marshal(elems)
		if err != nil {
			return "", fmt.Errorf("%s: cannot encode array operand: %w", b.dialect.backend, err)
		}
		return render(field, []string{b.bind(string(doc))}), nil
	}

	phs := make([]string, len(elems))
	for i, e := range elems {
		phs[i] = b.bind(e)
	}
	return render(field, phs), nil
}

// like renders a LIKE predicate, wrapping the bound value with the given
// prefix/suffix wildcards. The case rule is applied to the value only: the
// wildcards are punctuation and the pattern is built after folding, so a
// prefix search still anchors where the caller meant. When ci is set the
// column is compared through LOWER() too, and the pattern is folded a second
// time (harmless on top of valueCase, which never applies together with ci).
func (b *sqlBuilder) like(field, pre, post string, v *Value, vc ValueCase, ci bool) (string, error) {
	if v == nil {
		return "", fmt.Errorf("%s: LIKE requires a value on %s", b.dialect.backend, field)
	}
	s, ok := v.AsString()
	if !ok {
		return "", fmt.Errorf("%s: LIKE requires a string value on %s", b.dialect.backend, field)
	}
	pattern := pre + vc.apply(s) + post
	if ci && b.dialect.backend == postgresDialect.backend {
		// Postgres's own case-insensitive LIKE. Equivalent to folding both
		// sides through LOWER(), without needing to touch the column.
		return fmt.Sprintf("%s ILIKE %s", field, b.bind(pattern)), nil
	}
	return fmt.Sprintf("%s LIKE %s", foldColumn(field, ci), b.bind(foldArg(pattern, ci))), nil
}

// contains behaves differently by field type: array membership uses the
// dialect's array-contains form; a string "contains" is a %substring% LIKE.
func (b *sqlBuilder) contains(c *Config, field string, cond *Condition, vc ValueCase, ci bool) (string, error) {
	if f, ok := c.FieldByName(cond.Field); ok && f.Type == FieldArray {
		// The bound literal is one element, so it is read as the item type.
		arg, err := b.value(cond.Value, scalarTypeOf(f), vc)
		if err != nil {
			return "", err
		}
		if b.dialect.bindArrayWhole {
			doc, err := json.Marshal([]any{arg})
			if err != nil {
				return "", fmt.Errorf("%s: cannot encode array operand: %w", b.dialect.backend, err)
			}
			return b.dialect.arrayContains(field, []string{b.bind(string(doc))}), nil
		}
		return b.dialect.arrayContains(field, []string{b.bind(arg)}), nil
	}
	return b.like(field, "%", "%", cond.Value, vc, ci) // %value% substring match
}

// value converts a scalar AST Value into the Go argument bound to a
// placeholder, applying the field's case rule to the types that carry letters.
// A date string keeps its own spelling — it is a timestamp, not a word.
//
// The conversion follows the type the config declares, not the value's kind
// tag; the tag is the model's guess and may disagree (valuetype.go). Every
// payload read here was type-checked by the validator, so a failed assertion
// means the two stages disagree about the config — worth an error, not a
// silently bound zero value.
func (b *sqlBuilder) value(v *Value, declared FieldType, vc ValueCase) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("%s: operator requires a value", b.dialect.backend)
	}
	if v.Kind == KindRelativeDate { // no payload to read: unit/amount resolve against the clock
		t, err := resolveRelative(b.now, v.Unit, v.Amount)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.dialect.backend, err)
		}
		return t, nil
	}
	switch declared {
	case FieldString, FieldEnum:
		s, ok := v.AsString()
		if !ok {
			return nil, fmt.Errorf("%s: %s value is not text", b.dialect.backend, declared)
		}
		return vc.apply(s), nil
	case FieldDate:
		s, ok := v.AsString()
		if !ok {
			return nil, fmt.Errorf("%s: date value is not text", b.dialect.backend)
		}
		return s, nil
	case FieldNumber:
		f, ok := v.AsFloat()
		if !ok {
			return nil, fmt.Errorf("%s: number value is not numeric", b.dialect.backend)
		}
		return f, nil
	case FieldBoolean:
		bb, ok := v.AsBool()
		if !ok {
			return nil, fmt.Errorf("%s: boolean value is not a boolean", b.dialect.backend)
		}
		return bb, nil
	default:
		return nil, fmt.Errorf("%s: cannot bind a value for field type %q", b.dialect.backend, declared)
	}
}
