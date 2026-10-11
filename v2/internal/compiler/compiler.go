// Package compiler turns a SemanticPlan into a final PostgreSQL-style query
// string plus parameters, by a fixed grammar.
//
// Section 14 of the brief:
//
//	Query
//	├── SELECT
//	├── FROM
//	├── JOIN*
//	├── WHERE?
//	├── GROUP BY?
//	├── HAVING?
//	├── ORDER BY?
//	└── LIMIT?
//
//	"Every grammar node must be produced by deterministic code." ...
//	"PASS when compiler output is deterministic and cannot emit identifiers
//	outside the configured schema."
//
// THREE STAGES, each in its own file:
//
//	check.go   resolve every name in the plan against the registry and the
//	           schema graph, and refuse any plan whose SQL would be invalid or
//	           would silently answer a different question (fan-out traps).
//	lower.go   build grammar nodes from the checked plan. Decides SQL SHAPE:
//	           which paths become JOINs and which become EXISTS / COUNT
//	           subqueries, from configured cardinality (decision 015).
//	render.go  write the nodes as text, allocating $n placeholders in the
//	           order they appear.
//
// WHAT CAN REACH THE OUTPUT. Identifiers come only from the registry
// (Entity.Table, Field.Column), are re-checked against a strict pattern, and
// are always double-quoted. Table aliases are generated ("t0", "t1", ...).
// Keywords, operators and functions come from switch statements over the
// closed vocabularies. Values never appear in the SQL text: every one is a
// parameter (section 16.3). The only numbers written into the text are LIMIT,
// an int formatted by strconv. Nothing from a plan is ever spliced in as text.
//
// The compiler trusts nothing about its input. A plan from the planner and a
// plan written by hand for the gate go through the same checks.
package compiler

import (
	"fmt"
	"strconv"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// Query is a compiled query: SQL text with $n placeholders, and the values
// for them in order.
type Query struct {
	SQL    string
	Params []Param
}

// Param is one placeholder's value. Params[i] is $(i+1).
type Param struct {
	Index int
	Value plan.Value
}

// Driver returns the value in the Go type a PostgreSQL driver takes: int64
// for a whole number in int64's range, and otherwise the number's exact
// decimal text as a string; string, bool or time.Time for the other kinds.
// The experiment never calls a driver (section 16.9); this is the shape
// Phase 7's output contract hands to whoever does.
//
// NO float64 (decision 022 §2). A float64 parameter would round the user's
// literal on its way out even though every stage before kept it exact:
// 99999999999999999999999 would be sent as 1e23, and 0.1 as the binary
// fraction nearest to it. A driver sends a string parameter as text, and
// PostgreSQL parses the text into the compared column's numeric type, digit
// for digit. Before the gate, whole numbers were int64 only up to 2^53 (the
// float64-exact range) and everything else was float64.
func (p Param) Driver() any {
	v := p.Value
	switch v.Kind {
	case plan.NumberValue:
		if n, ok := v.Number.Int64(); ok {
			return n
		}
		return v.Number.String()
	case plan.StringValue:
		return v.String.Reveal()
	case plan.BoolValue:
		return v.Bool
	case plan.TimeValue:
		return v.Time
	}
	return nil
}

// Render prints the SQL and a parameter listing. String values appear only
// when reveal is set (decision 006); numbers, booleans and instants always.
func (q Query) Render(reveal bool) string {
	var s strings.Builder
	s.WriteString(q.SQL)
	if len(q.Params) > 0 {
		s.WriteString("\n\nparams:\n")
		for _, p := range q.Params {
			fmt.Fprintf(&s, "%d = %s (%s)\n", p.Index, p.Value.Render(reveal), p.Value.Kind)
		}
	}
	return s.String()
}

// String is the redacted rendering, so %v of a Query cannot leak.
func (q Query) String() string { return q.Render(false) }

// GoString covers %#v.
func (q Query) GoString() string { return "compiler.Query{" + q.Render(false) + "}" }

// Compile checks, lowers and renders a plan.
//
// The same plan always yields byte-identical SQL and identical parameters:
// every step iterates slices in plan order, aliases and placeholders are
// numbered in emission order, and no map order is ever observed.
func Compile(p plan.Plan, reg *config.SchemaRegistry, g *graph.Graph) (Query, error) {
	if reg == nil || g == nil {
		return Query{}, qferr.New(qferr.CodeInternal, "compiling needs a registry and a schema graph")
	}
	c, err := check(p, reg, g)
	if err != nil {
		return Query{}, err
	}
	stmt, err := lower(c, reg)
	if err != nil {
		return Query{}, err
	}
	// Section 16.7's whole-query limits: joins across all the plan's paths,
	// and nesting in the tree about to be rendered (limits.go).
	if err := checkShape(c, stmt, reg.Security()); err != nil {
		return Query{}, err
	}
	q, err := render(stmt)
	if err != nil {
		return Query{}, err
	}
	// Section 16.7's query-length limit, measured on the finished text.
	if max := reg.Security().MaxQueryLength; max > 0 && len(q.SQL) > max {
		return Query{}, qferr.New(qferr.CodeSecurityRejected,
			"the compiled query is %d characters, over the limit of %d", len(q.SQL), max)
	}
	// The last step: the finished text against the grammar's lexicon
	// (guard.go). A failure here is a compiler bug, refused, never returned.
	if err := outputGuard(q.SQL, len(q.Params), reg); err != nil {
		return Query{}, err
	}
	return q, nil
}

// identifierPattern is what may be emitted as an identifier. The config
// validator enforces the same pattern at load (Phase 1); this second check is
// deliberate. A registry can be built from a config that never went through
// Validate, and the compiler is the last point where that can be caught.
func quoteIdent(s string) (string, error) {
	if s == "" || len(s) > 63 {
		return "", qferr.New(qferr.CodeSecurityRejected, "an identifier from the config is empty or longer than 63 characters")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return "", qferr.New(qferr.CodeSecurityRejected,
				"an identifier from the config is not a plain identifier (%s); it is never emitted", qferr.Sensitive(s))
		}
	}
	return `"` + s + `"`, nil
}

// sqlOperator is the operator allowlist (section 16.4): the only comparison
// text the compiler writes.
func sqlOperator(op config.Operator) (string, bool) {
	switch op {
	case config.OpEq:
		return "=", true
	case config.OpNeq:
		return "<>", true
	case config.OpGt:
		return ">", true
	case config.OpGte:
		return ">=", true
	case config.OpLt:
		return "<", true
	case config.OpLte:
		return "<=", true
	case config.OpIn:
		return "IN", true
	case config.OpNin:
		return "NOT IN", true
	case config.OpLike:
		return "LIKE", true
	}
	return "", false
}

// sqlFunction is the function allowlist (section 16.5).
func sqlFunction(a config.Aggregation) (string, bool) {
	switch a {
	case config.AggCount:
		return "COUNT", true
	case config.AggSum:
		return "SUM", true
	case config.AggAvg:
		return "AVG", true
	case config.AggMin:
		return "MIN", true
	case config.AggMax:
		return "MAX", true
	}
	return "", false
}

func alias(n int) string { return "t" + strconv.Itoa(n) }
