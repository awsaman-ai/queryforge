package compiler

import (
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// Phase 8, section 16.7: the whole-query limits (decision 021). Each is tested
// at the limit (compiles) and one past it (refused, with its own reason).

func commerceWith(t *testing.T, change func(*config.SecurityPolicy)) (*config.SchemaRegistry, *graph.Graph) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", "commerce.json"))
	if err != nil {
		t.Fatal(err)
	}
	change(&cfg.Security)
	reg := config.NewSchemaRegistry(cfg)
	return reg, graph.New(reg)
}

// joins-and-subqueries reaches product and customer from order: two paths of
// one join each. Per path, maxJoins 1 allows both; the query as a whole needs
// two, so maxJoins 1 refuses it and 2 compiles it.
func TestJoinLimitIsQueryWide(t *testing.T) {
	p, err := loadPlan(t, "joins-and-subqueries")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		limit  int
		refuse bool
	}{{2, false}, {1, true}} {
		reg, g := commerceWith(t, func(s *config.SecurityPolicy) { s.MaxJoins = c.limit })
		q, err := Compile(p, reg, g)
		if !c.refuse {
			if err != nil {
				t.Errorf("maxJoins %d: %v", c.limit, err)
			}
			continue
		}
		if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected {
			t.Fatalf("maxJoins %d: err %v", c.limit, err)
		}
		if !strings.Contains(err.Error(), "needs 2 joins across all its paths, over the limit of 1") {
			t.Errorf("message does not state the reason: %v", err)
		}
		if q.SQL != "" {
			t.Error("a refusal carried SQL")
		}
	}
}

// exists-and-count reads orders in three subqueries and products in one, but
// its paths reach only two distinct tables. The limit counts the paths' union
// (2), not the lowered SQL's table reads (4): at maxJoins 2 it compiles. This
// pins decision 021's choice; the per-read count refused this reviewed golden
// under commerce's own limit of 3.
func TestJoinLimitCountsPathsNotSubqueryReads(t *testing.T) {
	p, err := loadPlan(t, "exists-and-count")
	if err != nil {
		t.Fatal(err)
	}
	reg, g := commerceWith(t, func(s *config.SecurityPolicy) { s.MaxJoins = 2 })
	q, err := Compile(p, reg, g)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(q.SQL, "FROM \"orders\""); n != 3 {
		t.Fatalf("premise: the SQL should read orders 3 times, reads it %d:\n%s", n, q.SQL)
	}
}

// The grammar never nests a subquery inside a subquery, so nesting is tested
// on hand-built trees: depth 1 is at the default limit, depth 2 is refused.
func TestNestingDepthLimit(t *testing.T) {
	sec := config.DefaultSecurityPolicy // maxNestingDepth 1
	c := &checked{}
	flat := &selectStmt{where: []predicate{comparePred{}}}
	one := &selectStmt{where: []predicate{existsPred{sub: subquery{inner: comparePred{}}}}}
	two := &selectStmt{having: []predicate{countPred{sub: subquery{inner: existsPred{sub: subquery{}}}}}}

	for _, ok := range []*selectStmt{flat, one} {
		if err := checkShape(c, ok, sec); err != nil {
			t.Errorf("depth within the limit refused: %v", err)
		}
	}
	err := checkShape(c, two, sec)
	if err == nil || qferr.CodeOf(err) != qferr.CodeSecurityRejected {
		t.Fatalf("depth 2 under limit 1: err %v", err)
	}
	if !strings.Contains(err.Error(), "nests subqueries 2 deep, over the limit of 1") {
		t.Errorf("message does not state the reason: %v", err)
	}
}

// strangePred is a predicate kind the nesting check has never heard of.
type strangePred struct{}

func (strangePred) isPredicate() {}

// A predicate kind the check does not know is a compiler bug. It is refused as
// INTERNAL_ERROR, never counted as flat. (err != nil is asserted separately:
// CodeOf(nil) is also INTERNAL_ERROR, decision 017 §4.)
func TestNestingCheckRefusesUnknownPredicates(t *testing.T) {
	err := checkShape(&checked{}, &selectStmt{where: []predicate{strangePred{}}}, config.DefaultSecurityPolicy)
	if err == nil {
		t.Fatal("an unknown predicate kind was counted as flat")
	}
	if qferr.CodeOf(err) != qferr.CodeInternal {
		t.Errorf("code %s", qferr.CodeOf(err))
	}
}

// Every reviewed Phase 6 golden stays within its config's limits: the new
// whole-query checks refused nothing the Phase 6 gate accepted.
func TestGoldensAreWithinTheWholeQueryLimits(t *testing.T) {
	for name, cfgName := range planFixtures {
		p, err := loadPlan(t, name)
		if err != nil {
			t.Fatal(err)
		}
		reg, g := registry(t, cfgName)
		if _, err := Compile(p, reg, g); err != nil {
			t.Errorf("%s on %s: %v", name, cfgName, err)
		}
	}
}

// Every PREFIX of a path is a joined table, counted once. Two conditions from
// customer, each two joins deep — orders then products, orders then payments
// — share their first join: the query needs 3 (orders, products, payments),
// while each path alone needs 2. At maxJoins 2 each path is within the graph's
// per-path bound, and the query is refused; at 3 it compiles. (Counting only
// whole paths would give 2 and let it through.) OR-joined: AND through one
// to-many relationship is refused as ambiguous (decision 017 §2).
func TestJoinLimitCountsEveryPrefixOnce(t *testing.T) {
	p, err := plan.Decode([]byte(`{
	  "root": "customer",
	  "select": [{ "field": "customer.name" }],
	  "where": [
	    { "kind": "compare", "field": "product.price", "via": ["customer_orders", "order_product"], "op": "gt", "values": [{ "number": 100 }] },
	    { "kind": "compare", "field": "payment.amount", "via": ["customer_orders", "order_payments"], "op": "gt", "values": [{ "number": 50 }] }
	  ],
	  "whereJoin": "or"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		limit  int
		refuse bool
	}{{3, false}, {2, true}} {
		cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", "ambiguity.json"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Security.MaxJoins = c.limit
		reg := config.NewSchemaRegistry(cfg)
		_, err = Compile(p, reg, graph.New(reg))
		if !c.refuse {
			if err != nil {
				t.Errorf("maxJoins %d: %v", c.limit, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "needs 3 joins across all its paths, over the limit of 2") {
			t.Errorf("maxJoins %d: %v", c.limit, err)
		}
	}
}
