package graph

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

func load(t *testing.T, name string) *Graph {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return New(config.NewSchemaRegistry(cfg))
}

// The brief's own picture (section 13): customer → order → product.
func TestCustomersWhoBoughtLaptopsPath(t *testing.T) {
	g := load(t, "commerce.json")
	p, err := g.Resolve("customer", "product", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "customer -[customer_orders]-> order -[order_product]-> product" {
		t.Fatalf("path = %s", got)
	}
	// Join keys in traversal order, both directions of declaration.
	want := []Step{
		{Relationship: "customer_orders", From: config.Endpoint{Entity: "customer", Field: "id"},
			To: config.Endpoint{Entity: "order", Field: "customer_id"}, Forward: true, ToMany: true},
		{Relationship: "order_product", From: config.Endpoint{Entity: "order", Field: "product_id"},
			To: config.Endpoint{Entity: "product", Field: "id"}, Forward: true, ToMany: false},
	}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Errorf("steps = %+v", p.Steps)
	}
	if !p.ToMany() {
		t.Error("customer → product fans out through orders")
	}
}

// Traversal against the declared direction flips the keys and the cardinality.
func TestReverseTraversal(t *testing.T) {
	g := load(t, "commerce.json")
	p, err := g.Resolve("product", "customer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "product -[order_product]-> order -[customer_orders]-> customer" {
		t.Fatalf("path = %s", p)
	}
	first := p.Steps[0]
	if first.Forward || !first.ToMany || first.From.Entity != "product" || first.From.Field != "id" {
		t.Errorf("first step = %+v; product → order is the reverse of a many-to-one, so it fans out", first)
	}
	last := p.Steps[1]
	if last.Forward || last.ToMany {
		t.Errorf("order → customer against a one-to-many is to-one: %+v", last)
	}
}

func TestSameEntityIsTheEmptyPath(t *testing.T) {
	g := load(t, "commerce.json")
	p, err := g.Resolve("order", "order", nil)
	if err != nil || p.Len() != 0 || p.ToMany() {
		t.Errorf("got %v, %v", p, err)
	}
}

// Required rejection: candidates that cannot be connected.
func TestDisconnectedEntitiesAreRelationshipNotFound(t *testing.T) {
	g := load(t, "multipath.json")
	_, err := g.Resolve("customer", "warehouse", nil)
	if qferr.CodeOf(err) != qferr.CodeRelationshipNotFound {
		t.Fatalf("got %v, want RELATIONSHIP_NOT_FOUND", err)
	}
	if !strings.Contains(err.Error(), "customer") || !strings.Contains(err.Error(), "warehouse") {
		t.Errorf("message should name both entities: %v", err)
	}
}

// The join limit is a hard bound: a path one join too long does not exist.
func TestJoinLimitBoundsTheSearch(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", "commerce.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Security.MaxJoins = 1
	g := New(config.NewSchemaRegistry(cfg))
	if _, err := g.Resolve("customer", "product", nil); qferr.CodeOf(err) != qferr.CodeRelationshipNotFound {
		t.Errorf("a two-join path under a one-join limit: got %v", err)
	}
	if _, err := g.Resolve("customer", "order", nil); err != nil {
		t.Errorf("a one-join path under a one-join limit: %v", err)
	}
}

// Required rejection: multiple join paths equally plausible and not
// disambiguated. Order reaches customer through billing AND shipping.
func TestEquallyPlausiblePathsAreAmbiguous(t *testing.T) {
	g := load(t, "multipath.json")
	_, err := g.Resolve("order", "customer", nil)
	if qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Fatalf("got %v, want AMBIGUOUS_MATCH", err)
	}
	for _, rel := range []string{"order_billing_customer", "order_shipping_customer"} {
		if !strings.Contains(err.Error(), rel) {
			t.Errorf("message should list %s: %v", rel, err)
		}
	}
}

// ...and a relationship the sentence mentions disambiguates.
func TestMentionedRelationshipPicksThePath(t *testing.T) {
	g := load(t, "multipath.json")
	for _, rel := range []string{"order_billing_customer", "order_shipping_customer"} {
		p, err := g.Resolve("order", "customer", []string{rel})
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !p.Uses(rel) || p.Len() != 1 {
			t.Errorf("%s: path %s", rel, p)
		}
	}
}

// A mention outranks length. Triangle a–b–c plus a direct a–c edge: the
// direct path is shorter, but a sentence naming r_ab asked for the way round.
func TestMentionOutranksLength(t *testing.T) {
	ep := func(e string) config.Endpoint { return config.Endpoint{Entity: e, Field: "id"} }
	cfg := &config.Config{
		Entities: []config.Entity{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		Relationships: []config.Relationship{
			{Name: "r_ab", From: ep("a"), To: ep("b"), Cardinality: config.OneToMany},
			{Name: "r_bc", From: ep("b"), To: ep("c"), Cardinality: config.OneToMany},
			{Name: "r_ac", From: ep("a"), To: ep("c"), Cardinality: config.OneToMany},
		},
		Security: config.SecurityPolicy{MaxJoins: 3},
	}
	g := New(config.NewSchemaRegistry(cfg))

	short, err := g.Resolve("a", "c", nil)
	if err != nil || short.String() != "a -[r_ac]-> c" {
		t.Fatalf("unprompted: %v, %v; want the direct edge", short, err)
	}
	named, err := g.Resolve("a", "c", []string{"r_ab"})
	if err != nil || named.String() != "a -[r_ab]-> b -[r_bc]-> c" {
		t.Errorf("with r_ab mentioned: %v, %v; want the path through it", named, err)
	}
}

// Every search is in a fixed order: the listing of tied paths must not depend
// on map iteration.
func TestPathsAreDeterministic(t *testing.T) {
	first := ""
	for i := 0; i < 30; i++ {
		g := load(t, "multipath.json")
		var keys []string
		for _, p := range g.Paths("order", "customer") {
			keys = append(keys, p.Key())
		}
		got := strings.Join(keys, ";")
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d: %s, first %s", i, got, first)
		}
	}
}

func TestRelatedEntityAndNeighbours(t *testing.T) {
	g := load(t, "commerce.json")
	if got := g.RelatedEntity("order", "customer_id"); !reflect.DeepEqual(got, []string{"customer"}) {
		t.Errorf("order.customer_id → %v", got)
	}
	if got := g.RelatedEntity("order", "amount"); len(got) != 0 {
		t.Errorf("order.amount is not a join key: %v", got)
	}
	if got := g.Neighbours("order"); !reflect.DeepEqual(got, []string{"customer", "product"}) {
		t.Errorf("neighbours = %v", got)
	}
}

// Walk is how the compiler turns a plan's relationship names back into keyed,
// directed steps. It must reproduce exactly what Resolve produces for the same
// route, in either direction of declaration.
func TestWalkReproducesResolvedPaths(t *testing.T) {
	g := load(t, "commerce.json")
	for _, c := range [][2]string{{"customer", "product"}, {"product", "customer"}, {"order", "customer"}} {
		want, err := g.Resolve(c[0], c[1], nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := g.Walk(c[0], want.Names())
		if err != nil {
			t.Fatalf("%s → %s: %v", c[0], c[1], err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s → %s: Walk = %+v, Resolve = %+v", c[0], c[1], got, want)
		}
	}
	empty, err := g.Walk("customer", nil)
	if err != nil || empty.Len() != 0 || empty.To != "customer" {
		t.Errorf("an empty walk is the root itself: %+v, %v", empty, err)
	}
}

// Every way a plan could name a join the graph would not produce is refused
// with SECURITY_REJECTED: the relationships are the join allowlist.
func TestWalkRefusesWhatTheGraphWouldNotProduce(t *testing.T) {
	g := load(t, "commerce.json")
	cases := []struct {
		name string
		from string
		via  []string
		want string
	}{
		{"unknown entity", "invoice", nil, "not configured"},
		{"unknown relationship", "customer", []string{"customer_invoices"}, "not configured"},
		{"relationship not touching the entity", "customer", []string{"order_product"}, "does not touch customer"},
		{"revisits an entity", "customer", []string{"customer_orders", "customer_orders"}, "only simple paths"},
		{"over the join limit", "customer", []string{"a", "b", "c", "d"}, "join limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := g.Walk(c.from, c.via)
			if qferr.CodeOf(err) != qferr.CodeSecurityRejected || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want SECURITY_REJECTED containing %q", err, c.want)
			}
		})
	}
	// The unknown names are input, not schema: redacted by default.
	_, err := g.Walk("customer", []string{"x_secret_rel"})
	if strings.Contains(err.Error(), "x_secret_rel") {
		t.Errorf("unknown relationship name echoed: %v", err)
	}
}

func TestPrefixAndFanOut(t *testing.T) {
	g := load(t, "commerce.json")
	p, _ := g.Walk("product", []string{"order_product", "customer_orders"})
	if p.FanOut() != 0 {
		t.Errorf("product → order fans out at step 0, got %d", p.FanOut())
	}
	q, _ := g.Walk("order", []string{"order_product"})
	if q.FanOut() != -1 {
		t.Errorf("order → product is to-one, got %d", q.FanOut())
	}
	pre := p.Prefix(1)
	if pre.To != "order" || pre.Len() != 1 || p.Prefix(0).To != "product" {
		t.Errorf("prefix = %+v", pre)
	}
}
