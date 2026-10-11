package compiler

import (
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/qferr"
)

// This file enforces the section 16.7 limits that are properties of the whole
// query rather than of one path or one condition: how many joins the query
// needs, and how deep its subqueries nest (Phase 8, decision 021).
//
// JOINS are counted on the plan's paths: the size of their union, a tree
// rooted at the query's root, where each distinct path prefix is one joined
// table. Two conditions over the same relationship cost one join, not two.
// The count is NOT taken from the lowered SQL, which re-reads a to-many path
// inside every EXISTS or COUNT subquery. That re-reading is how lowering keeps
// rows from fanning out (decision 015), not something the request asked for;
// counting it would charge "orders with a laptop OR more than 3 orders OR no
// orders" three joins for one relationship. That alternative was measured: it
// refuses a reviewed Phase 6 golden (exists-and-count, 4 reads > commerce's 3).
//
// The graph already bounds every single path by maxJoins. The union is at
// least as long as the longest path, so this is the stricter, query-wide bound.
//
// NESTING is measured on the lowered statement, the tree render writes: it is
// a property of the SQL shape, which the plan does not have.

// countJoins is the size of the union of every path the checked plan uses.
func countJoins(c *checked) int {
	seen := map[string]bool{}
	add := func(p graph.Path) {
		for n := 1; n <= p.Len(); n++ {
			seen[p.Prefix(n).Key()] = true
		}
	}
	addField := func(f *field) {
		if f != nil {
			add(f.path)
		}
	}
	for i := range c.selects {
		addField(&c.selects[i])
	}
	for _, a := range c.aggs {
		add(a.path)
		addField(a.field)
	}
	for i := range c.where {
		add(c.where[i].path)
		add(c.where[i].field.path)
	}
	for _, h := range c.having {
		add(h.agg.path)
		addField(h.agg.field)
	}
	for _, o := range c.order {
		addField(o.field)
	}
	return len(seen)
}

// depth is the deepest subquery nesting in a statement: 0 when flat, 1 for an
// EXISTS or COUNT subquery in WHERE or HAVING, and so on. It has to know every
// predicate kind: an unknown one is a compiler bug, refused rather than
// counted as flat.
func depth(s *selectStmt) (int, error) {
	deepest := 0
	for _, list := range [][]predicate{s.where, s.having} {
		for _, p := range list {
			d, err := predicateDepth(p)
			if err != nil {
				return 0, err
			}
			if d > deepest {
				deepest = d
			}
		}
	}
	return deepest, nil
}

func predicateDepth(p predicate) (int, error) {
	switch p := p.(type) {
	case nil, comparePred, rangePred:
		return 0, nil
	case existsPred:
		d, err := predicateDepth(p.sub.inner)
		return 1 + d, err
	case countPred:
		d, err := predicateDepth(p.sub.inner)
		return 1 + d, err
	}
	return 0, qferr.New(qferr.CodeInternal, "the compiler met a predicate kind its nesting check does not know (%T)", p)
}

// checkShape refuses a query over the query-wide join limit or the nesting
// limit.
func checkShape(c *checked, s *selectStmt, sec config.SecurityPolicy) error {
	if max, n := sec.MaxJoins, countJoins(c); max > 0 && n > max {
		return qferr.New(qferr.CodeSecurityRejected,
			"the query needs %d joins across all its paths, over the limit of %d", n, max)
	}
	d, err := depth(s)
	if err != nil {
		return err
	}
	if max := sec.MaxNestingDepth; max > 0 && d > max {
		return qferr.New(qferr.CodeSecurityRejected,
			"the query nests subqueries %d deep, over the limit of %d", d, max)
	}
	return nil
}
