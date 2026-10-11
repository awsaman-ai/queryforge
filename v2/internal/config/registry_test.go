package config

import (
	"sync"
	"testing"
)

func testRegistry(t *testing.T) *SchemaRegistry {
	t.Helper()
	cfg, err := Load(fixture("commerce.json"))
	if err != nil {
		t.Fatalf("commerce.json rejected: %v", err)
	}
	return NewSchemaRegistry(cfg)
}

func TestRegistryLookups(t *testing.T) {
	r := testRegistry(t)

	if _, ok := r.Entity("customer"); !ok {
		t.Error("customer should be indexed")
	}
	if _, ok := r.Entity("invoice"); ok {
		t.Error("invoice is not configured and must not resolve")
	}

	// Exact-name lookup only. Semantic matching belongs to the retrieval layer;
	// if the registry started guessing, the two layers would disagree about what
	// exists.
	if _, ok := r.Entity("Customer"); ok {
		t.Error("entity lookup should be exact, not case-insensitive")
	}

	f, ok := r.Field("customer", "total_spent")
	if !ok {
		t.Fatal("customer.total_spent should be indexed")
	}
	if f.Type != TypeNumber {
		t.Errorf("total_spent type = %q, want %q", f.Type, TypeNumber)
	}
	if _, ok := r.Field("order", "total_spent"); ok {
		t.Error("total_spent belongs to customer, not order")
	}
}

// TestRegistryOrderIsStable matters more than it looks: corpus order, candidate
// order and debug output are all derived from these slices, and a Go map walk
// would make every one of them vary per run.
func TestRegistryOrderIsStable(t *testing.T) {
	r := testRegistry(t)

	entities := r.Entities()
	want := []string{"customer", "order", "product"}
	if len(entities) != len(want) {
		t.Fatalf("got %d entities, want %d", len(entities), len(want))
	}
	for i, name := range want {
		if entities[i].Name != name {
			t.Errorf("entities[%d] = %q, want %q", i, entities[i].Name, name)
		}
	}

	fields := r.FieldsOf("customer")
	wantFields := []string{"country", "created_at", "id", "name", "status", "total_spent"}
	if len(fields) != len(wantFields) {
		t.Fatalf("got %d customer fields, want %d", len(fields), len(wantFields))
	}
	for i, name := range wantFields {
		if fields[i].Name != name {
			t.Errorf("fields[%d] = %q, want %q", i, fields[i].Name, name)
		}
	}

	// Repeated calls must agree, which is what rules out incidental ordering
	// that happens to be stable within one call.
	for i := 0; i < 20; i++ {
		again := r.Entities()
		for j := range again {
			if again[j].Name != entities[j].Name {
				t.Fatalf("entity order varies between calls at %d", j)
			}
		}
	}
}

// TestRelationshipsTouchingIsBidirectional covers the distinction that decides
// whether Phase 5 can answer "customers who bought laptops": which side the
// config calls "from" is an authoring detail, not a constraint on traversal.
func TestRelationshipsTouchingIsBidirectional(t *testing.T) {
	r := testRegistry(t)

	if got := len(r.RelationshipsFrom("order")); got != 1 {
		t.Errorf("order is the declared from-side of 1 relationship, got %d", got)
	}
	// order sits between customer and product, so both edges touch it.
	if got := len(r.RelationshipsTouching("order")); got != 2 {
		t.Errorf("order is touched by 2 relationships, got %d", got)
	}
	if got := len(r.RelationshipsFrom("product")); got != 0 {
		t.Errorf("product is never a from-side, got %d", got)
	}
	if got := len(r.RelationshipsTouching("product")); got != 1 {
		t.Errorf("product is touched by 1 relationship, got %d", got)
	}
}

// A self-referencing relationship must not be indexed twice under one entity,
// or a traversal would see the same edge as two distinct options and could
// report a spurious ambiguity.
func TestSelfRelationshipIndexedOnce(t *testing.T) {
	cfg := &Config{
		Entities: []Entity{{Name: "employee", Table: "employees"}},
		Fields: []Field{
			{Entity: "employee", Name: "id", Type: TypeNumber},
			{Entity: "employee", Name: "manager_id", Type: TypeNumber},
		},
		Relationships: []Relationship{{
			Name:        "reports_to",
			From:        Endpoint{Entity: "employee", Field: "manager_id"},
			To:          Endpoint{Entity: "employee", Field: "id"},
			Cardinality: ManyToOne,
		}},
	}
	cfg.Normalize()
	if err := Validate(cfg); err != nil {
		t.Fatalf("a self-referencing relationship should be valid: %v", err)
	}

	r := NewSchemaRegistry(cfg)
	if got := len(r.RelationshipsTouching("employee")); got != 1 {
		t.Errorf("the self-edge should be indexed once, got %d", got)
	}
}

// TestRegistryAccessorsReturnCopies is the concurrency contract: a registry is
// built once and shared by every request without a lock, so no accessor may
// hand out something a caller can mutate.
func TestRegistryAccessorsReturnCopies(t *testing.T) {
	r := testRegistry(t)

	fields := r.FieldsOf("customer")
	if len(fields) == 0 {
		t.Fatal("customer should have fields")
	}
	fields[0] = nil
	if again := r.FieldsOf("customer"); again[0] == nil {
		t.Error("FieldsOf handed out the index's backing array")
	}

	rels := r.RelationshipsTouching("order")
	rels[0] = nil
	if again := r.RelationshipsTouching("order"); again[0] == nil {
		t.Error("RelationshipsTouching handed out the index's backing array")
	}

	patterns := r.QueryPatterns()
	patterns[0] = nil
	if again := r.QueryPatterns(); again[0] == nil {
		t.Error("QueryPatterns handed out the index's backing array")
	}

	ops := r.OperatorPhrases(OpGt)
	if len(ops) == 0 {
		t.Fatal("gt should have phrases")
	}
	ops[0] = "clobbered"
	if again := r.OperatorPhrases(OpGt); again[0] == "clobbered" {
		t.Error("OperatorPhrases handed out the config's backing array")
	}
}

// TestRegistryIsConcurrencySafe runs the read paths under -race. A registry is
// immutable after construction, and this is what proves nothing lazily mutates
// it on first read.
func TestRegistryIsConcurrencySafe(t *testing.T) {
	r := testRegistry(t)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Entity("customer")
				r.Field("customer", "country")
				r.FieldsOf("customer")
				r.RelationshipsTouching("order")
				r.QueryPatterns()
				r.ConfiguredOperators()
				r.ResolveValueAlias("customer", "country", "india")
				r.Security()
			}
		}()
	}
	wg.Wait()
}

// TestResolveValueAlias covers the case-insensitive lookup. Users type "india",
// and the validator has already rejected aliases that differ only by case, so
// this cannot silently pick one of two plausible targets.
func TestResolveValueAlias(t *testing.T) {
	r := testRegistry(t)

	cases := []struct {
		value string
		want  string
		found bool
	}{
		{"India", "IN", true},
		{"india", "IN", true},
		{"  INDIA  ", "IN", true},
		{"United Kingdom", "UK", true},
		{"Atlantis", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := r.ResolveValueAlias("customer", "country", c.value)
		if ok != c.found || got != c.want {
			t.Errorf("ResolveValueAlias(%q) = (%q, %v), want (%q, %v)", c.value, got, ok, c.want, c.found)
		}
	}

	// Enum aliases resolve onto declared values, which is what the validator
	// enforces and what lets resolution refuse an unknown value later.
	if got, ok := r.ResolveValueAlias("product", "category", "laptops"); !ok || got != "laptop" {
		t.Errorf(`ResolveValueAlias(product.category, "laptops") = (%q, %v), want ("laptop", true)`, got, ok)
	}

	if _, ok := r.ResolveValueAlias("customer", "nonexistent", "x"); ok {
		t.Error("an unknown field must not resolve an alias")
	}
}

// ConfiguredOperators, not the full vocabulary, is what the Phase 2 corpus
// should be built from: an operator with no phrases is unreachable from natural
// language, which is a legitimate way for a config to disable one.
func TestConfiguredSetsReflectPhrasesNotVocabulary(t *testing.T) {
	cfg, err := Load(fixture("customers.json"))
	if err != nil {
		t.Fatalf("customers.json rejected: %v", err)
	}
	r := NewSchemaRegistry(cfg)

	if got, want := len(r.ConfiguredOperators()), 3; got != want {
		t.Errorf("customers.json configures %d operators, got %d", want, got)
	}
	if got := len(Operators()); got <= len(r.ConfiguredOperators()) {
		t.Errorf("the vocabulary (%d) should be larger than what this config reaches (%d)",
			got, len(r.ConfiguredOperators()))
	}
	if got := len(r.ConfiguredAggregations()); got != 0 {
		t.Errorf("customers.json configures no aggregations, got %d", got)
	}
}
