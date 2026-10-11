package compiler

import (
	"strconv"
	"strings"

	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// The text layout is fixed, so identical statements are identical bytes:
//
//	SELECT <col>, <col>
//	FROM "table" AS "t0"
//	LEFT JOIN "other" AS "t1" ON "t1"."key" = "t0"."key"
//	WHERE <predicate>
//	  AND <predicate>
//	GROUP BY <col>
//	HAVING <predicate>
//	ORDER BY <expr> DESC NULLS LAST
//	LIMIT 10;
//
// A subquery predicate opens on its own line and is indented four spaces past
// the clause it sits in. Placeholders are numbered in the order they are
// written, so $1 is always the first value a reader meets.
//
// NULLS LAST on every ORDER BY key, in both directions. PostgreSQL puts NULLs
// FIRST when sorting descending, so "top 10 customers by spending" would
// otherwise open with the customers whose spending is unknown.

type writer struct {
	b      strings.Builder
	params []Param
}

func render(s *selectStmt) (Query, error) {
	w := &writer{}
	if err := w.stmt(s, ""); err != nil {
		return Query{}, err
	}
	w.b.WriteString(";")
	return Query{SQL: w.b.String(), Params: w.params}, nil
}

// param records a value and writes its placeholder.
func (w *writer) param(v plan.Value) {
	w.params = append(w.params, Param{Index: len(w.params) + 1, Value: v})
	w.b.WriteString("$" + strconv.Itoa(len(w.params)))
}

func (w *writer) stmt(s *selectStmt, ind string) error {
	w.b.WriteString("SELECT ")
	if s.distinct {
		w.b.WriteString("DISTINCT ")
	}
	for i, c := range s.columns {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.expr(c)
	}
	w.b.WriteString("\n" + ind + "FROM " + s.from.table + " AS " + s.from.alias)
	for _, j := range s.joins {
		w.b.WriteString("\n" + ind)
		w.join(j)
	}
	if err := w.predicates("WHERE", s.where, s.whereOr, ind); err != nil {
		return err
	}
	if len(s.groupBy) > 0 {
		w.b.WriteString("\n" + ind + "GROUP BY ")
		for i, g := range s.groupBy {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.expr(g)
		}
	}
	if err := w.predicates("HAVING", s.having, s.havingOr, ind); err != nil {
		return err
	}
	if len(s.orderBy) > 0 {
		w.b.WriteString("\n" + ind + "ORDER BY ")
		for i, o := range s.orderBy {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.expr(o.e)
			if o.desc {
				w.b.WriteString(" DESC NULLS LAST")
			} else {
				w.b.WriteString(" ASC NULLS LAST")
			}
		}
	}
	if s.limit > 0 {
		w.b.WriteString("\n" + ind + "LIMIT " + strconv.Itoa(s.limit))
	}
	return nil
}

func (w *writer) join(j joinClause) {
	if j.left {
		w.b.WriteString("LEFT ")
	}
	w.b.WriteString("JOIN " + j.table.table + " AS " + j.table.alias + " ON ")
	w.col(j.on)
	w.b.WriteString(" = ")
	w.col(j.to)
}

func (w *writer) predicates(clause string, ps []predicate, or bool, ind string) error {
	for i, p := range ps {
		switch {
		case i == 0:
			w.b.WriteString("\n" + ind + clause + " ")
		case or:
			w.b.WriteString("\n" + ind + "   OR ")
		default:
			w.b.WriteString("\n" + ind + "  AND ")
		}
		if err := w.pred(p, ind); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) pred(p predicate, ind string) error {
	switch p := p.(type) {
	case comparePred:
		w.expr(p.left)
		w.b.WriteString(" " + p.op + " ")
		switch {
		case p.list:
			w.b.WriteString("(")
			for i, v := range p.values {
				if i > 0 {
					w.b.WriteString(", ")
				}
				w.param(v)
			}
			w.b.WriteString(")")
		case p.like:
			w.param(p.values[0])
			w.b.WriteString(` ESCAPE '\'`)
		default:
			w.param(p.values[0])
		}
	case rangePred:
		if p.negate {
			w.b.WriteString("NOT ")
		}
		w.b.WriteString("(")
		w.col(p.col)
		w.b.WriteString(" >= ")
		w.param(plan.Instant(p.start))
		w.b.WriteString(" AND ")
		w.col(p.col)
		w.b.WriteString(" < ")
		w.param(plan.Instant(p.end))
		w.b.WriteString(")")
	case existsPred:
		if p.negate {
			w.b.WriteString("NOT ")
		}
		w.b.WriteString("EXISTS ")
		return w.sub(p.sub, "1", ind)
	case countPred:
		if err := w.sub(p.sub, "COUNT(*)", ind); err != nil {
			return err
		}
		w.b.WriteString(" " + p.op + " ")
		w.param(p.value)
	default:
		return qferr.New(qferr.CodeCompilationError, "a predicate has no rendering rule")
	}
	return nil
}

// sub writes "(\n    SELECT <what>\n    FROM ...\n  )" at the given indent.
func (w *writer) sub(s subquery, what, ind string) error {
	in := ind + "    "
	w.b.WriteString("(\n" + in + "SELECT " + what)
	w.b.WriteString("\n" + in + "FROM " + s.from.table + " AS " + s.from.alias)
	for _, j := range s.joins {
		w.b.WriteString("\n" + in)
		w.join(j)
	}
	w.b.WriteString("\n" + in + "WHERE ")
	w.col(s.correlate[0])
	w.b.WriteString(" = ")
	w.col(s.correlate[1])
	if s.inner != nil {
		w.b.WriteString("\n" + in + "  AND ")
		if err := w.pred(s.inner, in); err != nil {
			return err
		}
	}
	w.b.WriteString("\n" + ind + "  )")
	return nil
}

func (w *writer) expr(e expr) {
	switch e := e.(type) {
	case columnRef:
		w.col(e)
	case aggExpr:
		w.b.WriteString(e.fn + "(")
		if e.arg == nil {
			w.b.WriteString("*")
		} else {
			w.col(*e.arg)
		}
		w.b.WriteString(")")
	}
}

func (w *writer) col(c columnRef) { w.b.WriteString(c.alias + "." + c.column) }
