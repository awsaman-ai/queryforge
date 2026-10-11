package corpus

import (
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"

	"queryforge.local/v2experiment/internal/qferr"
)

func registry(t *testing.T, name string) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s rejected: %v", name, err)
	}
	return config.NewSchemaRegistry(cfg)
}

func build(t *testing.T, name string) []Entry {
	t.Helper()
	return Build(registry(t, name))
}

// TestCorpusIsDeterministic is the brief's first Phase 2 test. It matters more
// than it looks: the index is cached and keyed by a fingerprint of the corpus,
// so a corpus whose order drifted between runs would invalidate its own cache
// on every startup and silently pay the embedding cost forever.
func TestCorpusIsDeterministic(t *testing.T) {
	first := build(t, "commerce.json")
	if len(first) == 0 {
		t.Fatal("corpus is empty")
	}

	for i := 0; i < 20; i++ {
		again := build(t, "commerce.json")
		if len(again) != len(first) {
			t.Fatalf("corpus size varies: %d then %d", len(first), len(again))
		}
		for j := range again {
			if again[j].ID != first[j].ID {
				t.Fatalf("entry %d is %q then %q", j, first[j].ID, again[j].ID)
			}
			if strings.Join(again[j].Texts(), "|") != strings.Join(first[j].Texts(), "|") {
				t.Fatalf("entry %s surfaces vary between runs", first[j].ID)
			}
		}
	}
}

func TestEveryKindIsProduced(t *testing.T) {
	entries := build(t, "commerce.json")

	counts := map[Kind]int{}
	for _, e := range entries {
		counts[e.Kind]++
	}
	for _, kind := range []Kind{
		KindEntity, KindField, KindOperator, KindAggregation,
		KindValueAlias, KindQueryPattern, KindRelationship,
	} {
		if counts[kind] == 0 {
			t.Errorf("no %s entries were produced", kind)
		}
	}

	// commerce.json has 3 entities, 15 fields, 9 operators, 5 aggregations,
	// 2 relationships and 4 patterns. Pinned so a change to the builder that
	// drops or duplicates a category is visible.
	if got, want := counts[KindEntity], 3; got != want {
		t.Errorf("entity entries = %d, want %d", got, want)
	}
	if got, want := counts[KindField], 15; got != want {
		t.Errorf("field entries = %d, want %d", got, want)
	}
	if got, want := counts[KindRelationship], 2; got != want {
		t.Errorf("relationship entries = %d, want %d", got, want)
	}
}

func TestIDsAreUniqueAndStable(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range build(t, "commerce.json") {
		if seen[e.ID] {
			t.Errorf("duplicate entry id %q", e.ID)
		}
		seen[e.ID] = true
	}

	// The id format is a contract: a persisted index stores ids and maps
	// results back through them across process boundaries.
	for _, want := range []string{
		"entity:customer",
		"field:customer.total_spent",
		"operator:gt",
		"aggregation:sum",
		"relationship:customer_orders",
		"pattern:top_n",
	} {
		if !seen[want] {
			t.Errorf("expected an entry with id %q", want)
		}
	}
}

// The identifier is not what users say. Embedding "total_spent" verbatim would
// hand the model a token it has never seen as a word, which is exactly the
// vocabulary mismatch the corpus exists to bridge.
func TestFieldNamesAreHumanized(t *testing.T) {
	for _, e := range build(t, "commerce.json") {
		if e.ID != "field:customer.total_spent" {
			continue
		}
		var nameSurface string
		for _, s := range e.Surfaces {
			if s.Kind == SurfaceName {
				nameSurface = s.Text
			}
		}
		if nameSurface != "total spent" {
			t.Errorf("name surface = %q, want %q", nameSurface, "total spent")
		}
		if strings.Contains(e.Passage(), "total_spent") {
			t.Errorf("the passage should not contain the raw identifier:\n%s", e.Passage())
		}
		return
	}
	t.Fatal("customer.total_spent not found in the corpus")
}

// Value aliases and enum values are config VALUES. They are embedded — that is
// how "customers from India" finds the country field — but every one of them
// must carry the sensitive marker so nothing renders them by default.
func TestValueSurfacesAreMarkedSensitive(t *testing.T) {
	var values int
	for _, e := range build(t, "commerce.json") {
		if e.Kind != KindValueAlias {
			continue
		}
		values++
		for _, s := range e.Surfaces {
			if !s.Sensitive {
				t.Errorf("entry %s has an unmarked value surface %q", e.ID, s.Text)
			}
		}
	}
	if values == 0 {
		t.Fatal("commerce.json has value aliases and enum values; none were produced")
	}

	// And nothing else should be marked sensitive: over-marking would redact
	// the structural names that make a message actionable.
	for _, e := range build(t, "commerce.json") {
		if e.Kind == KindValueAlias {
			continue
		}
		if e.HasSensitiveSurface() {
			t.Errorf("entry %s (%s) should hold no sensitive surfaces", e.ID, e.Kind)
		}
	}
}

func TestEnumValuesAndAliasesBothBecomeEntries(t *testing.T) {
	ids := map[string]bool{}
	for _, e := range build(t, "commerce.json") {
		ids[e.ID] = true
	}

	// An alias the user might say, and a stored value they might also say.
	if !ids["value:product.category:alias:laptops"] {
		t.Error("the alias 'laptops' should be retrievable")
	}
	if !ids["value:product.category:enum:laptop"] {
		t.Error("the enum value 'laptop' should be retrievable")
	}
	if !ids["value:customer.status:enum:suspended"] {
		t.Error("the enum value 'suspended' should be retrievable")
	}
}

// A synonym list that repeats the entry's own name would embed it twice and
// quietly double its weight.
func TestSynonymsDoNotRepeatTheName(t *testing.T) {
	for _, e := range build(t, "commerce.json") {
		if e.Kind != KindEntity || e.Entity != "customer" {
			continue
		}
		// customer.json lists "customer" among its own synonyms.
		var count int
		for _, s := range e.Surfaces {
			if strings.EqualFold(s.Text, "customer") {
				count++
			}
		}
		if count > 1 {
			t.Errorf("the name 'customer' appears %d times in the surfaces of %s", count, e.ID)
		}
		return
	}
	t.Fatal("customer entity not found")
}

func TestPassageComposition(t *testing.T) {
	for _, e := range build(t, "commerce.json") {
		if e.ID != "field:customer.total_spent" {
			continue
		}
		p := e.Passage()
		for _, want := range []string{
			"customer total spent",
			"Total amount of money spent by the customer",
			"spending",
			"a number field of customer",
		} {
			if !strings.Contains(strings.ToLower(p), strings.ToLower(want)) {
				t.Errorf("passage is missing %q:\n%s", want, p)
			}
		}
		return
	}
	t.Fatal("customer.total_spent not found")
}

// Metadata is what Phase 4 scores compatibility against, so it has to survive
// the trip from config into the corpus.
func TestFieldMetadataIsCarried(t *testing.T) {
	for _, e := range build(t, "commerce.json") {
		switch e.ID {
		case "field:customer.total_spent":
			if e.Meta.FieldType != config.TypeNumber {
				t.Errorf("total_spent type = %q, want number", e.Meta.FieldType)
			}
			if len(e.Meta.Operators) == 0 {
				t.Error("total_spent should carry its allowed operators")
			}
		case "field:order.customer_id":
			if e.Meta.Filterable || e.Meta.Selectable {
				t.Error("order.customer_id is configured false for both flags")
			}
		case "relationship:customer_orders":
			if e.Meta.Cardinality != config.OneToMany {
				t.Errorf("cardinality = %q, want one-to-many", e.Meta.Cardinality)
			}
		}
	}
}

// The minimal config must still produce a usable corpus: no relationships, no
// patterns, no aggregations, and that is fine.
func TestMinimalConfigProducesACorpus(t *testing.T) {
	entries := build(t, "customers.json")
	if len(entries) == 0 {
		t.Fatal("customers.json produced no corpus")
	}
	counts := map[Kind]int{}
	for _, e := range entries {
		counts[e.Kind]++
	}
	if counts[KindEntity] != 1 || counts[KindField] != 4 {
		t.Errorf("expected 1 entity and 4 fields, got %d and %d", counts[KindEntity], counts[KindField])
	}
	if counts[KindRelationship] != 0 || counts[KindQueryPattern] != 0 {
		t.Error("customers.json declares no relationships or patterns")
	}
	if counts[KindValueAlias] != 3 {
		t.Errorf("customers.json has 3 country aliases, got %d", counts[KindValueAlias])
	}
}

// REGRESSION (found in Phase 4): value entries embed the config value in their
// ID, and Phase 2's --search printed the ID verbatim while redacting the
// matched column beside it.
func TestDisplayIDWithholdsValues(t *testing.T) {
	cases := []struct{ id, want string }{
		{"value:customer.country:alias:India", "value:customer.country:alias:" + qferr.Placeholder},
		{"value:customer.status:enum:suspended", "value:customer.status:enum:" + qferr.Placeholder},
		// A value containing colons is withheld whole, not partly.
		{"value:x.y:alias:a:b:c", "value:x.y:alias:" + qferr.Placeholder},
		// Malformed: withhold everything past the prefix rather than guess.
		{"value:oops", "value:" + qferr.Placeholder},
		// Structure is untouched.
		{"field:customer.country", "field:customer.country"},
		{"entity:customer", "entity:customer"},
		{"operator:gt", "operator:gt"},
	}
	for _, c := range cases {
		if got := DisplayID(c.id, false); got != c.want {
			t.Errorf("DisplayID(%q) = %q, want %q", c.id, got, c.want)
		}
		if got := DisplayID(c.id, true); got != c.id {
			t.Errorf("DisplayID(%q, reveal) = %q, want the id unchanged", c.id, got)
		}
	}
}

// Every value entry Build produces, from every valid fixture, redacts.
func TestEveryValueEntryIDRedacts(t *testing.T) {
	for _, name := range []string{"commerce.json", "customers.json", "ambiguity.json"} {
		for _, e := range build(t, name) {
			if e.Kind != KindValueAlias {
				continue
			}
			shown := DisplayID(e.ID, false)
			for _, s := range e.Surfaces {
				if strings.Contains(shown, s.Text) {
					t.Errorf("%s: DisplayID %q still contains the value %q", name, shown, s.Text)
				}
			}
			if e.Meta.ValueTarget != "" && strings.Contains(shown, e.Meta.ValueTarget) {
				t.Errorf("%s: DisplayID %q contains the value target", name, shown)
			}
		}
	}
}
