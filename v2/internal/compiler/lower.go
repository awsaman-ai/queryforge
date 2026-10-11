package compiler

import (
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// ---------------------------------------------------------------------------
// Grammar nodes: section 14's tree, and nothing else.
//
// Every string field below holds either an identifier that came out of
// quoteIdent, a generated alias, or a keyword chosen by a switch. None holds
// text from a plan or a sentence. Values are plan.Value, and are written only
// as placeholders.
// ---------------------------------------------------------------------------

// selectStmt is Query: SELECT, FROM, JOIN*, WHERE?, GROUP BY?, HAVING?,
// ORDER BY?, LIMIT?.
type selectStmt struct {
	distinct bool
	columns  []expr
	from     tableRef
	joins    []joinClause
	where    []predicate
	whereOr  bool
	groupBy  []expr
	having   []predicate
	havingOr bool
	orderBy  []orderItem
	limit    int
}

type tableRef struct{ table, alias string } // both already quoted

type columnRef struct{ alias, column string } // both already quoted

// joinClause is `[LEFT] JOIN table AS alias ON alias.col = prev.col`.
type joinClause struct {
	left  bool
	table tableRef
	on    columnRef // the new table's key
	to    columnRef // the key already in scope
}

type expr interface{ isExpr() }

func (columnRef) isExpr() {}

// aggExpr is FUNC(column) or COUNT(*).
type aggExpr struct {
	fn  string // from sqlFunction
	arg *columnRef
}

func (aggExpr) isExpr() {}

type predicate interface{ isPredicate() }

// comparePred is `left op $n`, `left IN ($n, ...)`, or `left LIKE $n ESCAPE '\'`.
type comparePred struct {
	left   expr
	op     string // from sqlOperator
	values []plan.Value
	list   bool
	like   bool
}

// rangePred is `(col >= $a AND col < $b)`, optionally under NOT.
type rangePred struct {
	col        columnRef
	start, end time.Time
	negate     bool
}

// subquery is the fixed-shape correlated subquery used for EXISTS and COUNT:
// the path from the root, joined inner, correlated on its first step.
type subquery struct {
	from      tableRef
	joins     []joinClause
	correlate [2]columnRef // inner key = outer key
	inner     predicate    // nil for a bare EXISTS or COUNT
}

type existsPred struct {
	sub    subquery
	negate bool
}

type countPred struct {
	sub   subquery
	op    string
	value plan.Value
}

func (comparePred) isPredicate() {}
func (rangePred) isPredicate()   {}
func (existsPred) isPredicate()  {}
func (countPred) isPredicate()   {}

type orderItem struct {
	e    expr
	desc bool
}

// ---------------------------------------------------------------------------
// Lowering
// ---------------------------------------------------------------------------

// lowerer builds one statement. Aliases are numbered in the order tables are
// introduced, which is plan order, so they are deterministic.
type lowerer struct {
	reg     *config.SchemaRegistry
	next    int
	stmt    *selectStmt
	joinFor map[string]string // path-prefix key → alias of the table it reaches
}

// lower builds the statement for a checked plan.
//
// THE SHAPE RULES (decision 015):
//
//   - FROM is the root. A path that stays to-one is a chain of LEFT JOINs:
//     each root row meets at most one row, so the root's rows are neither lost
//     nor repeated, and a condition on the joined column filters exactly as it
//     would on the root (a NULL from a missing row never satisfies it).
//   - The output's to-many path (the "domain", when aggregates range over
//     related rows) is joined INNER: the aggregate is over related rows that
//     exist, so COUNT(*) does not count a NULL-extended row.
//   - A condition whose path fans out becomes a correlated EXISTS over its own
//     path, so it restricts root rows without repeating them. A count through a
//     relationship becomes a correlated COUNT(*) subquery. Neither can
//     multiply the outer rows, and both combine with OR as freely as a plain
//     comparison — which GROUP BY / HAVING could not.
func lower(c *checked, reg *config.SchemaRegistry) (*selectStmt, error) {
	l := &lowerer{reg: reg, stmt: &selectStmt{distinct: c.distinct, whereOr: c.whereOr, havingOr: c.havingOr, limit: c.limit},
		joinFor: map[string]string{}}

	rootTable, err := quoteIdent(c.root.Table)
	if err != nil {
		return nil, err
	}
	l.stmt.from = tableRef{table: rootTable, alias: l.newAlias()}
	l.joinFor[graph.Path{From: c.root.Name, To: c.root.Name}.Key()] = l.stmt.from.alias

	// The domain first, so its steps are INNER from the moment they exist and
	// every later path sharing a prefix reuses those joins.
	if c.domain != nil {
		if _, err := l.join(*c.domain, true); err != nil {
			return nil, err
		}
	}

	for _, f := range c.selects {
		col, err := l.column(f)
		if err != nil {
			return nil, err
		}
		l.stmt.columns = append(l.stmt.columns, col)
		if len(c.aggs) > 0 {
			l.stmt.groupBy = append(l.stmt.groupBy, col)
		}
	}
	aggs := make([]aggExpr, len(c.aggs))
	for i, a := range c.aggs {
		e, err := l.aggregate(a)
		if err != nil {
			return nil, err
		}
		aggs[i] = e
		l.stmt.columns = append(l.stmt.columns, e)
	}

	for _, w := range c.where {
		p, err := l.condition(w)
		if err != nil {
			return nil, err
		}
		l.stmt.where = append(l.stmt.where, p)
	}

	for _, h := range c.having {
		e, err := l.aggregate(h.agg)
		if err != nil {
			return nil, err
		}
		op, _ := sqlOperator(h.op)
		l.stmt.having = append(l.stmt.having, comparePred{left: e, op: op, values: []plan.Value{h.value}})
	}

	for _, o := range c.order {
		var e expr
		if o.agg >= 0 {
			e = aggs[o.agg]
		} else {
			col, err := l.column(*o.field)
			if err != nil {
				return nil, err
			}
			e = col
		}
		l.stmt.orderBy = append(l.stmt.orderBy, orderItem{e: e, desc: o.desc})
	}
	return l.stmt, nil
}

func (l *lowerer) newAlias() string {
	a := `"` + alias(l.next) + `"`
	l.next++
	return a
}

// join makes sure every step of p is joined in the outer query and returns
// the alias of the table p reaches. Shared prefixes are joined once.
func (l *lowerer) join(p graph.Path, inner bool) (string, error) {
	at := l.joinFor[p.Prefix(0).Key()]
	for i, st := range p.Steps {
		key := p.Prefix(i + 1).Key()
		if a, ok := l.joinFor[key]; ok {
			at = a
			continue
		}
		jc, err := l.joinClause(st, at, !inner)
		if err != nil {
			return "", err
		}
		l.stmt.joins = append(l.stmt.joins, jc)
		l.joinFor[key] = jc.table.alias
		at = jc.table.alias
	}
	return at, nil
}

// joinClause builds the join for one step, from the table at prevAlias.
func (l *lowerer) joinClause(st graph.Step, prevAlias string, left bool) (joinClause, error) {
	ent, ok := l.reg.Entity(st.To.Entity)
	if !ok {
		return joinClause{}, qferr.New(qferr.CodeCompilationError, "entity %s vanished from the registry", st.To.Entity)
	}
	table, err := quoteIdent(ent.Table)
	if err != nil {
		return joinClause{}, err
	}
	toCol, err := l.keyColumn(st.To)
	if err != nil {
		return joinClause{}, err
	}
	fromCol, err := l.keyColumn(st.From)
	if err != nil {
		return joinClause{}, err
	}
	a := l.newAlias()
	return joinClause{left: left, table: tableRef{table: table, alias: a},
		on: columnRef{alias: a, column: toCol}, to: columnRef{alias: prevAlias, column: fromCol}}, nil
}

// keyColumn resolves a relationship endpoint to its column. Join keys are
// fields like any other, so they go through the registry too.
func (l *lowerer) keyColumn(ep config.Endpoint) (string, error) {
	f, ok := l.reg.Field(ep.Entity, ep.Field)
	if !ok {
		return "", qferr.New(qferr.CodeCompilationError, "join key %s.%s is not a configured field", ep.Entity, ep.Field)
	}
	return quoteIdent(f.Column)
}

// column resolves a field reached through the outer query's joins.
func (l *lowerer) column(f field) (columnRef, error) {
	a, err := l.join(f.path, false)
	if err != nil {
		return columnRef{}, err
	}
	col, err := quoteIdent(f.field.Column)
	if err != nil {
		return columnRef{}, err
	}
	return columnRef{alias: a, column: col}, nil
}

func (l *lowerer) aggregate(a aggregate) (aggExpr, error) {
	fn, ok := sqlFunction(a.fn)
	if !ok {
		return aggExpr{}, qferr.New(qferr.CodeSecurityRejected, "function %s is not in the allowlist", a.fn)
	}
	if a.field == nil {
		// COUNT(*) of the rows at the end of its path: the root's own rows,
		// or the domain's (check.go allows nothing else), both already joined.
		return aggExpr{fn: fn}, nil
	}
	col, err := l.column(*a.field)
	if err != nil {
		return aggExpr{}, err
	}
	return aggExpr{fn: fn, arg: &col}, nil
}

func (l *lowerer) condition(c condition) (predicate, error) {
	switch c.kind {
	case plan.Exists:
		sub, _, err := l.subquery(c.path)
		if err != nil {
			return nil, err
		}
		return existsPred{sub: sub, negate: c.negate}, nil

	case plan.Count:
		sub, _, err := l.subquery(c.path)
		if err != nil {
			return nil, err
		}
		op, _ := sqlOperator(c.op)
		return countPred{sub: sub, op: op, value: c.values[0]}, nil
	}

	// compare and range: on the outer joins if the path stays to-one, inside
	// an EXISTS over the path if it fans out.
	if c.path.FanOut() < 0 {
		col, err := l.column(c.field)
		if err != nil {
			return nil, err
		}
		return l.leaf(c, col), nil
	}
	sub, last, err := l.subquery(c.path)
	if err != nil {
		return nil, err
	}
	col, err := quoteIdent(c.field.field.Column)
	if err != nil {
		return nil, err
	}
	sub.inner = l.leaf(c, columnRef{alias: last, column: col})
	return existsPred{sub: sub}, nil
}

func (l *lowerer) leaf(c condition, col columnRef) predicate {
	if c.kind == plan.Range {
		return rangePred{col: col, start: c.start, end: c.end, negate: c.negate}
	}
	op, _ := sqlOperator(c.op)
	p := comparePred{left: col, op: op, values: c.values,
		list: c.op == config.OpIn || c.op == config.OpNin, like: c.op == config.OpLike}
	if p.like {
		// The compiler owns the wildcards (vocabulary.go: "controlled substring
		// match, not a user-supplied pattern"). The user's %, _ and \ are
		// escaped, then wrapped, and the whole pattern is ONE parameter.
		p.values = []plan.Value{plan.Str("%" + escapeLike(c.values[0].String.Reveal()) + "%")}
	}
	return p
}

// subquery builds the correlated subquery over a path from the root: the
// path's tables joined INNER, the first one correlated with the outer root.
// It returns the alias of the path's last table. Subquery aliases continue the
// outer numbering, so every alias in the statement is unique.
func (l *lowerer) subquery(p graph.Path) (subquery, string, error) {
	first := p.Steps[0]
	ent, ok := l.reg.Entity(first.To.Entity)
	if !ok {
		return subquery{}, "", qferr.New(qferr.CodeCompilationError, "entity %s vanished from the registry", first.To.Entity)
	}
	table, err := quoteIdent(ent.Table)
	if err != nil {
		return subquery{}, "", err
	}
	innerKey, err := l.keyColumn(first.To)
	if err != nil {
		return subquery{}, "", err
	}
	outerKey, err := l.keyColumn(first.From)
	if err != nil {
		return subquery{}, "", err
	}
	a := l.newAlias()
	sub := subquery{from: tableRef{table: table, alias: a},
		correlate: [2]columnRef{{alias: a, column: innerKey}, {alias: l.stmt.from.alias, column: outerKey}}}
	at := a
	for _, st := range p.Steps[1:] {
		jc, err := l.joinClause(st, at, false)
		if err != nil {
			return subquery{}, "", err
		}
		sub.joins = append(sub.joins, jc)
		at = jc.table.alias
	}
	return sub, at, nil
}

// escapeLike escapes LIKE's metacharacters with the ESCAPE character the
// compiler declares ('\'), backslash first so the escapes are not escaped.
func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
