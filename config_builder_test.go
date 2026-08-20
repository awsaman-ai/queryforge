package queryforge

// Tests for the contract between the QueryForge config builder and this loader.
//
// The builder used to be docs/config-builder.html, shipped inside this repo; it
// now lives at queryforge.amtry.in/config-builder.html, on the QueryForge
// website (awsaman-ai/queryforge_service), alongside the rest of the site's
// navigation. It is still an HTML page: nothing about it can be compiled or
// type-checked against Config, so the one thing that could quietly rot is the
// file format it emits. If a key is renamed here, or a new structural rule
// lands in finalize(), the page keeps producing yesterday's JSON and the first
// person to notice is a user whose downloaded config will not load.
//
// The fixtures in docs/testdata/ are therefore not hand-written: each one is
// verbatim output from the builder, saved as it came out. These tests assert
// that the real loader accepts them, that a config imported into the builder
// and re-emitted is unchanged, and that the configs the builder marks as broken
// are exactly the ones the loader rejects. Regenerate the fixtures from the page
// whenever its output changes.
//
// What this file can no longer check, now that the page lives in a different
// repo: that the page exists, stays self-contained, and mirrors every
// AllOperators/FieldType/secretPrefixes value and every Field/ModelConfig JSON
// key (formerly TestBuilderPageShipsWithTheLibrary, which read
// docs/config-builder.html directly via os.ReadFile and reflected over Field{}
// and ModelConfig{}). That verification has to happen by hand on this repo's
// side now — diff config.go/ast.go against the page's HELP/KNOWN_FIELD/
// KNOWN_MODEL/ALL_OPERATORS catalogues after any schema change — or by adding
// an equivalent check in queryforge_service, which does not currently depend
// on this module.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuilderOutputLoads parses every config the builder produced. These files
// between them use every key the loader knows, so a rename or a new required
// rule fails here rather than in a user's download.
func TestBuilderOutputLoads(t *testing.T) {
	paths := []string{
		"docs/testdata/builder_minimal.config.json",          // the smallest config that loads
		"docs/testdata/builder_full.config.json",             // every documented key, all six field types
		"docs/testdata/builder_roundtrip_orders.config.json", // examples/orders.config.json, imported and re-emitted
		"docs/testdata/builder_nested.config.json",           // embedded documents and arrays of sub-documents
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			if _, err := LoadConfig(p); err != nil {
				t.Fatalf("builder output does not load: %v", err)
			}
		})
	}
}

// TestBuilderFullConfigSemantics spot-checks that the builder's choices survive
// the trip with their meaning intact. Parsing successfully is not enough: a flag
// serialized in the wrong place would still parse, and would still be wrong.
func TestBuilderFullConfigSemantics(t *testing.T) {
	c, err := LoadConfig("docs/testdata/builder_full.config.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// An explicit false must arrive as false, not as an unset pointer taking
	// the type default (which for returnable would be true — the exact
	// direction that leaks a hidden column).
	ssn, ok := c.FieldByName("ssn")
	if !ok {
		t.Fatal("field ssn missing")
	}
	if ssn.EffectiveQueryable() || ssn.EffectiveFilterable() || ssn.EffectiveReturnable() {
		t.Errorf("ssn should be fully hidden, got queryable=%v filterable=%v returnable=%v",
			ssn.EffectiveQueryable(), ssn.EffectiveFilterable(), ssn.EffectiveReturnable())
	}

	// valueCase must arrive on the two fields that declare it and on no other,
	// in both directions: the key is the physical case of the stored column, so
	// leaking it onto a neighbouring field would silently recase that column's
	// values, and losing it would silently stop recasing this one's.
	name, ok := c.FieldByName("name")
	if !ok {
		t.Fatal("field name missing")
	}
	if name.ValueCase != CaseLower {
		t.Errorf("name.valueCase = %q, want %q", name.ValueCase, CaseLower)
	}
	department, ok := c.FieldByName("department")
	if !ok {
		t.Fatal("field department missing")
	}
	if department.ValueCase != CaseUpper {
		t.Errorf("department.valueCase = %q, want %q", department.ValueCase, CaseUpper)
	}
	for _, other := range []string{"salary", "active", "hiredAt", "skills"} {
		f, found := c.FieldByName(other) // not "ok": that would shadow the one above
		if !found {
			t.Fatalf("field %s missing", other)
		}
		if f.ValueCase != CaseAsIs {
			t.Errorf("%s.valueCase = %q, want no rule at all", other, f.ValueCase)
		}
	}

	// caseInsensitive must arrive true on the one field that declares it and
	// false everywhere else — the same leak-in-either-direction risk valueCase
	// carries just above, since it too changes what the built query does.
	tenantID, ok := c.FieldByName("tenantId")
	if !ok {
		t.Fatal("field tenantId missing")
	}
	if !tenantID.CaseInsensitive {
		t.Error("tenantId.caseInsensitive = false, want true")
	}
	for _, other := range []string{"name", "department", "salary", "active", "hiredAt"} {
		f, found := c.FieldByName(other)
		if !found {
			t.Fatalf("field %s missing", other)
		}
		if f.CaseInsensitive {
			t.Errorf("%s.caseInsensitive = true, want false", other)
		}
	}

	// A flag left on "default" must emit nothing, so the library keeps deciding
	// by type. An array is not sortable by default.
	scores, ok := c.FieldByName("scores")
	if !ok {
		t.Fatal("field scores missing")
	}
	if scores.Sortable != nil {
		t.Error("scores.sortable was left on default and must not be serialized")
	}
	if scores.EffectiveSortable() {
		t.Error("an array field must not be sortable by default")
	}
	if scores.ItemType != FieldNumber {
		t.Errorf("scores.itemType = %q, want number", scores.ItemType)
	}
	// Numeric bounds are emitted for an array of numbers because validate.go
	// checks them against the element type.
	if scores.Validators == nil || scores.Validators.Max == nil || *scores.Validators.Max != 100 {
		t.Error("scores validators.max = 100 did not survive")
	}

	// An empty operator list must be omitted entirely, which means "inherit the
	// type defaults" — never "permit nothing".
	if len(scores.Operators) != 0 {
		t.Errorf("scores.operators should be absent, got %v", scores.Operators)
	}
	if len(scores.EffectiveOperators()) == 0 {
		t.Error("an absent operators list must fall back to the type defaults")
	}

	// An array of enums keeps its domain. validate.go checks array elements
	// against the element type, so `values` matters here just as much as it does
	// on a plain enum field — and a builder that only offered `values` for
	// type: enum would silently drop it.
	channels, ok := c.FieldByName("channels")
	if !ok {
		t.Fatal("field channels missing")
	}
	if channels.ItemType != FieldEnum {
		t.Errorf("channels.itemType = %q, want enum", channels.ItemType)
	}
	if len(channels.Values) != 3 {
		t.Errorf("channels enum domain = %v, want 3 values", channels.Values)
	}

	// A narrowed whitelist must be exactly what was ticked.
	dept, _ := c.FieldByName("department")
	if got := dept.EffectiveOperators(); len(got) != 4 || !dept.AllowsOperator(OpIn) || dept.AllowsOperator(OpContains) {
		t.Errorf("department operators = %v, want the four ticked ones only", got)
	}
	if len(dept.Values) != 3 {
		t.Errorf("department enum domain = %v, want 3 values", dept.Values)
	}

	// Per-backend physical names, including one that differs per backend.
	if got := c.PhysicalName("hiredAt", "sql"); got != "hired_at" {
		t.Errorf("sql physical name = %q, want hired_at", got)
	}
	if got := c.PhysicalName("hiredAt", "mongo"); got != "hiredAt" {
		t.Errorf("mongo physical name = %q, want hiredAt", got)
	}
	// An unmapped field falls back to its logical name.
	if got := c.PhysicalName("active", "mongo"); got != "active" {
		t.Errorf("unmapped field should fall back to the logical name, got %q", got)
	}

	// Custom backends use the generic `name` key.
	if got := c.Backends["mybackend"].Source(); got != "employees" {
		t.Errorf("custom backend source = %q, want employees", got)
	}
	if got := c.Backends["es"].Source(); got != "employees-v2" {
		t.Errorf("es backend source = %q, want employees-v2", got)
	}

	// The fallback chain, and the keyless final hop.
	if len(c.Models) != 2 {
		t.Fatalf("models chain length = %d, want 2", len(c.Models))
	}
	if c.Models[1].APIKeyEnv != "" {
		t.Error("the keyless Ollama entry must not carry an apiKeyEnv")
	}
	// jsonMode is a tri-state: true, false, and absent must stay distinguishable.
	if !c.Model.EffectiveJSONMode() {
		t.Error("model.jsonMode: true did not survive")
	}
	if c.Models[0].EffectiveJSONMode() || c.Models[0].JSONMode == nil {
		t.Error("models[0].jsonMode: false must be present and false")
	}
	if c.Models[1].JSONMode != nil {
		t.Error("an unset jsonMode must be absent, not false")
	}

	// Synonyms must resolve case-insensitively, which is what the builder's
	// collision warning is about.
	if f, ok := c.ResolveSynonym("Hire Date"); !ok || f.Name != "hiredAt" {
		t.Error("synonym \"hire date\" should resolve to hiredAt")
	}

	// Policy.
	if c.Policy.MaxNestingDepth != 5 || len(c.Policy.DenyRegexOn) != 1 {
		t.Errorf("policy did not survive: %+v", c.Policy)
	}
	if c.Defaults.Limit != 50 || c.Defaults.MaxLimit != 500 {
		t.Errorf("defaults did not survive: %+v", c.Defaults)
	}
}

// TestBuilderNestedConfigCompiles closes the loop on the nesting UI. The
// fixture is what the builder emits for a Mongo collection with an embedded
// address and two arrays of sub-documents; this asserts that the file it writes
// actually produces the same-element query the page promises. A builder that
// emitted elemMatch in the wrong place, or dropped it, would still load — and
// would silently return the cross-element rows the whole feature exists to
// prevent.
func TestBuilderNestedConfigCompiles(t *testing.T) {
	c, err := LoadConfig("docs/testdata/builder_nested.config.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The embedded document carries a dot path and no array declaration.
	city, ok := c.FieldByName("city")
	if !ok {
		t.Fatal("field city missing")
	}
	if city.ElemMatch != "" {
		t.Errorf("city.elemMatch = %q, want empty — it is a plain embedded document", city.ElemMatch)
	}
	if got := c.PhysicalName("city", "mongo"); got != "shippingAddress.city" {
		t.Errorf("city mongo path = %q, want shippingAddress.city", got)
	}

	// The array fields split into the two halves $elemMatch needs.
	arr, rel, nested := c.MongoElemMatch("itemWidth")
	if !nested || arr != "items" || rel != "dims.w" {
		t.Errorf("itemWidth = (%q,%q,%v), want (items, dims.w, true)", arr, rel, nested)
	}

	// Two conditions on one array must compile to one $elemMatch.
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemQty", OpGte, vNum(2)),
	)
	if err := Validate(q, c); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"qty":{"$gte":2},"sku":"ABC"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestBuilderRoundTripPreservesConfig proves the builder's import path is
// lossless. The fixture is examples/orders.config.json after a trip through
// stateFromConfig -> buildConfig in the page, so if the two parse to the same
// Config, editing an existing file in the builder cannot silently drop a
// setting the author already had.
func TestBuilderRoundTripPreservesConfig(t *testing.T) {
	original, err := LoadConfig("examples/orders.config.json")
	if err != nil {
		t.Fatalf("load original: %v", err)
	}
	rebuilt, err := LoadConfig("docs/testdata/builder_roundtrip_orders.config.json")
	if err != nil {
		t.Fatalf("load round-tripped: %v", err)
	}

	// Compare the parsed structs by re-marshaling: that normalizes key order and
	// ignores the unexported lookup indexes, while still catching any field
	// whose value or presence changed.
	a, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	b, err := json.Marshal(rebuilt)
	if err != nil {
		t.Fatalf("marshal round-tripped: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("round-trip changed the config:\n original: %s\n rebuilt : %s", a, b)
	}
}

// TestBuilderInvalidOutputIsRejected pins the agreement in the other direction.
// Each fixture is a config the builder's own validation panel marks with a hard
// error; the loader must agree, or the page is telling users a file is broken
// when it loads fine (or worse, the reverse).
func TestBuilderInvalidOutputIsRejected(t *testing.T) {
	cases := []struct {
		file string // fixture under docs/testdata/invalid/
		want string // substring the loader's error must contain
	}{
		{"pasted_key.json", "looks like an API key"},
		{"env_name.json", "not a valid environment variable name"},
		{"no_entity.json", "entity is required"},
		{"no_fields.json", "at least one field is required"},
		{"dup_field.json", "duplicate field"},
		{"enum_no_values.json", "must list values"},
		{"unknown_operator.json", "unknown operator"},
		{"elemmatch_mismatch.json", "must sit inside that array"},
		{"bad_mongo_path.json", "invalid mongo path"},
		{"valuecase_wrong_type.json", "not strings"},
		{"caseinsensitive_wrong_type.json", "applies to string fields only"},
		{"caseinsensitive_with_valuecase.json", "cannot both apply"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := LoadConfig(filepath.Join("docs/testdata/invalid", tc.file))
			if err == nil {
				t.Fatal("loader accepted a config the builder flags as broken")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// The pasted-secret error must never echo the value back: reprinting
			// it is how the secret reaches the logs in the first place.
			if tc.file == "pasted_key.json" && strings.Contains(err.Error(), "AIza") {
				t.Error("error message repeated the secret")
			}
		})
	}
}

// The page-level checks used to live here as TestBuilderPageShipsWithTheLibrary:
// that the file exists, stays self-contained (no network fetch — the normal
// case is opening it straight from disk), keeps its DOM-free logic block
// identifiable, and — via reflection over Field{} and ModelConfig{} — that
// every JSON key, every AllOperators value, every FieldType and every entry in
// secretPrefixes is mirrored in the page's own catalogues. All of that read
// docs/config-builder.html directly, which no longer exists in this repo now
// that the page lives at queryforge.amtry.in/config-builder.html. See the
// package comment above for what replaces it.
