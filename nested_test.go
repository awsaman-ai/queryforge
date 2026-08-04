package queryforge

import (
	"encoding/json"
	"strings"
	"testing"
)

// nestedConfigJSON exercises every nesting shape the config can express:
//   - a plain embedded document (address.city) — one sub-document, so a dot path
//     is already exact and no elemMatch is declared;
//   - three fields inside the items[] array of sub-documents, including one at a
//     deeper relative path (items.dims.w);
//   - a second array (payments[]) so grouping is proven to be per-array;
//   - an ordinary top-level field, to prove nothing else is disturbed.
const nestedConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","DELIVERED"],
     "mapping":{"sql":"status","mongo":"status"}},
    {"name":"city","type":"string",
     "mapping":{"sql":"address_city","mongo":"address.city"}},
    {"name":"itemSku","type":"string","operators":["equals","contains","in"],
     "mapping":{"sql":"item_sku","mongo":"items.sku"},"elemMatch":"items"},
    {"name":"itemPrice","type":"number",
     "mapping":{"sql":"item_price","mongo":"items.price"},"elemMatch":"items"},
    {"name":"itemWidth","type":"number",
     "mapping":{"mongo":"items.dims.w"},"elemMatch":"items"},
    {"name":"payMethod","type":"string","operators":["equals","in"],
     "mapping":{"mongo":"payments.method"},"elemMatch":"payments"},
    {"name":"payAmount","type":"number",
     "mapping":{"mongo":"payments.amount"},"elemMatch":"payments"}
  ],
  "defaults":{"limit":50}
}`

func nestedConfig(t *testing.T) *Config { return mustParse(t, nestedConfigJSON) }

// filterJSON renders just the filter document, key-sorted by encoding/json, so
// a test can pin the exact structure in one readable string.
func filterJSON(t *testing.T, mq *MongoQuery) string {
	t.Helper()
	b, err := json.Marshal(mq.Filter)
	if err != nil {
		t.Fatalf("marshal filter: %v", err)
	}
	return string(b)
}

// TestMongoEmbeddedDocumentDotPath covers the simple case: a field inside a
// single embedded document needs no elemMatch, and its dot path becomes the
// filter key verbatim.
func TestMongoEmbeddedDocumentDotPath(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("city", OpEquals, vStr("Pune"))

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"address.city":"Pune"}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchGroupsSiblings is the defect this feature exists to prevent.
// Two predicates on the same array must land in ONE $elemMatch; the dot-path
// rendering {"items.sku":…,"items.price":…} would match a document whose sku
// lives on one element and whose price lives on another.
func TestMongoElemMatchGroupsSiblings(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemPrice", OpGt, vNum(100)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"price":{"$gt":100},"sku":"ABC"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
	// The old, wrong shape must be gone entirely.
	if strings.Contains(got, "items.sku") || strings.Contains(got, "items.price") {
		t.Errorf("filter still uses cross-element dot paths: %s", got)
	}
}

// TestMongoElemMatchSinglePredicate pins the one-predicate case. $elemMatch with
// a single criterion is equivalent to the dot path, but emitting it
// unconditionally keeps the output shape independent of how many predicates the
// sentence happened to produce.
func TestMongoElemMatchSinglePredicate(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("itemSku", OpEquals, vStr("ABC"))

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"sku":"ABC"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchDeepRelativePath proves the relative path keeps its own
// dots: items.dims.w under items must address dims.w inside the element.
func TestMongoElemMatchDeepRelativePath(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemWidth", OpGte, vNum(10)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"dims.w":{"$gte":10},"sku":"ABC"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchMergesRangeOnOneKey covers two operators on the same
// relative key: they fold into a single operator document (a range), rather than
// one silently overwriting the other.
func TestMongoElemMatchMergesRangeOnOneKey(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemPrice", OpGte, vNum(100)),
		comp("itemPrice", OpLte, vNum(500)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"price":{"$gte":100,"$lte":500}}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchConflictKeepsBothPredicates is the adversarial case: two
// equality predicates on the same key cannot merge into one key. Dropping either
// would return rows the caller did not ask for, so both must survive — inside
// the $elemMatch, preserving the same-element meaning even though the result is
// unsatisfiable.
func TestMongoElemMatchConflictKeepsBothPredicates(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("A")),
		comp("itemSku", OpEquals, vStr("B")),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"$and":[{"sku":"B"}],"sku":"A"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
	// Both values must still be present; a merge that kept only one would be a
	// silently wider query.
	if !strings.Contains(got, `"A"`) || !strings.Contains(got, `"B"`) {
		t.Errorf("a predicate was dropped: %s", got)
	}
}

// TestMongoElemMatchRepeatedOperatorKeepsBoth is the same hazard one level down:
// $gt twice on one key cannot merge into one operator document.
func TestMongoElemMatchRepeatedOperatorKeepsBoth(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemPrice", OpGt, vNum(100)),
		comp("itemPrice", OpGt, vNum(200)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"$and":[{"price":{"$gt":200}}],"price":{"$gt":100}}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchPerArray proves grouping is keyed by array path: two arrays
// produce two independent $elemMatch documents, never one merged blob.
func TestMongoElemMatchPerArray(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemPrice", OpGt, vNum(100)),
		comp("payMethod", OpEquals, vStr("CARD")),
		comp("payAmount", OpGte, vNum(50)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"items":{"$elemMatch":{"price":{"$gt":100},"sku":"ABC"}},` +
		`"payments":{"$elemMatch":{"amount":{"$gte":50},"method":"CARD"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchMixedWithPlainFields proves ordinary top-level predicates
// pass through the fold untouched and still merge into the same document.
func TestMongoElemMatchMixedWithPlainFields(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("city", OpEquals, vStr("Pune")),
		comp("itemPrice", OpGt, vNum(100)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"address.city":"Pune","items":{"$elemMatch":{"price":{"$gt":100},"sku":"ABC"}},"status":"DELIVERED"}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchUnderOrIsNotGrouped guards the boundary of the feature. Only
// AND means "the same element"; folding under OR would turn "an element with A,
// or an element with B" into "an element with A or B" — a different, narrower
// question.
func TestMongoElemMatchUnderOrIsNotGrouped(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = or(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemPrice", OpGt, vNum(100)),
	)

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"$or":[{"items":{"$elemMatch":{"sku":"ABC"}}},{"items":{"$elemMatch":{"price":{"$gt":100}}}}]}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoElemMatchUnderNot pins negation: $nor over the element predicate,
// i.e. "no element matches", which is the correct reading of NOT here.
func TestMongoElemMatchUnderNot(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = not(comp("itemSku", OpEquals, vStr("ABC")))

	got := filterJSON(t, genMongo(t, c, q))
	want := `{"$nor":[{"items":{"$elemMatch":{"sku":"ABC"}}}]}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// TestMongoNestedProjectionAndSortUseFullPath proves projection and sort keep
// the full dot path: $elemMatch is a filter construct only, and a projection
// keyed "sku" would name a field that does not exist at the document root.
func TestMongoNestedProjectionAndSortUseFullPath(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Select = []string{"itemSku", "city"}
	q.Sort = []SortSpec{{Field: "itemPrice", Dir: "DESC"}}

	mq := genMongo(t, c, q)
	if mq.Projection["items.sku"] != 1 || mq.Projection["address.city"] != 1 {
		t.Errorf("projection = %v, want full dot paths", mq.Projection)
	}
	if len(mq.Sort) != 1 || mq.Sort[0].Field != "items.price" || mq.Sort[0].Order != -1 {
		t.Errorf("sort = %v, want items.price desc", mq.Sort)
	}
}

// TestNestedOutputIsDeterministic guards the merge helpers, which walk Go maps.
// Unsorted iteration would make the $and fallback order vary between runs and
// break byte-comparison of generated queries (and any cache keyed on them).
func TestNestedOutputIsDeterministic(t *testing.T) {
	c := nestedConfig(t)
	build := func() string {
		q := NewQuery("Order")
		q.Filter = and(
			comp("itemSku", OpEquals, vStr("A")),
			comp("itemSku", OpEquals, vStr("B")),
			comp("itemPrice", OpGt, vNum(1)),
			comp("itemPrice", OpGt, vNum(2)),
			comp("payMethod", OpEquals, vStr("CARD")),
		)
		return filterJSON(t, genMongo(t, c, q))
	}
	first := build()
	for i := 0; i < 50; i++ { // enough runs that random map order would show
		if got := build(); got != first {
			t.Fatalf("run %d differs:\n got: %s\nfirst: %s", i, got, first)
		}
	}
}

// TestNestedFieldsDoNotAffectSQL pins the agreed division of labour: elemMatch
// is Mongo-only. SQL uses the field's own sql mapping (a flat column) and never
// sees a dot path.
func TestNestedFieldsDoNotAffectSQL(t *testing.T) {
	c := nestedConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemPrice", OpGt, vNum(100)),
	)

	r := genSQL(t, c, q)
	want := "SELECT * FROM orders WHERE (item_sku = $1 AND item_price > $2) LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL = %q, want %q", r.SQL, want)
	}
}

// TestNestedFieldsAreInvisibleToTheModel pins that nesting is a physical-layer
// concern. The prompt must advertise logical names only — leaking "items.sku"
// would invite the model to invent sibling paths that the config never declared.
func TestNestedFieldsAreInvisibleToTheModel(t *testing.T) {
	c := nestedConfig(t)
	prompt := NewPlanner(c, nil).SystemPrompt(fixedNow)

	for _, physical := range []string{"items.sku", "items.price", "address.city", "$elemMatch", "elemMatch"} {
		if strings.Contains(prompt, physical) {
			t.Errorf("system prompt leaks physical path %q:\n%s", physical, prompt)
		}
	}
	if !strings.Contains(prompt, "itemSku") { // the logical name must be there
		t.Error("system prompt omits the logical field name itemSku")
	}
}

// TestExplainReportsSameElementGrouping pins the readback. "sku ABC and price
// over 100" is ambiguous in prose; since the compiler resolves it to one
// element, the explanation must say so rather than leave the reader guessing.
func TestExplainReportsSameElementGrouping(t *testing.T) {
	c := nestedConfig(t)

	q := NewQuery("Order")
	q.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("itemPrice", OpGt, vNum(100)),
	)
	got := Explain(q, c)
	if !strings.Contains(got, "Conditions on items apply to the same array element.") {
		t.Errorf("explain omits the same-element note:\n%s", got)
	}

	// A single predicate groups nothing, so the note must stay off — an
	// explanation that always fires teaches the reader to ignore it.
	single := NewQuery("Order")
	single.Filter = and(
		comp("itemSku", OpEquals, vStr("ABC")),
		comp("status", OpEquals, vEnum("DELIVERED")),
	)
	if s := Explain(single, c); strings.Contains(s, "same array element") {
		t.Errorf("explain adds a same-element note where nothing was grouped:\n%s", s)
	}

	// Ordinary configs must read exactly as before.
	plain := genConfig(t)
	if s := Explain(canonicalQuery(), plain); strings.Contains(s, "same array element") {
		t.Errorf("same-element note leaked into a config with no nested fields:\n%s", s)
	}
}

// TestConfigRejectsBadNestedPaths is the adversarial config sweep: every one of
// these would otherwise load cleanly and then build a filter that matches
// nothing, with no error to explain the empty result.
func TestConfigRejectsBadNestedPaths(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  string // substring the error must contain
	}{
		{
			name:  "elemMatch is not a prefix of the mongo path",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"items.sku"},"elemMatch":"lines"}`,
			want:  "must sit inside that array",
		},
		{
			name:  "elemMatch equals the whole path, leaving nothing relative",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"items"},"elemMatch":"items"}`,
			want:  "must sit inside that array",
		},
		{
			name:  "partial segment is not a path prefix",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"itemsold.sku"},"elemMatch":"items"}`,
			want:  "must sit inside that array",
		},
		{
			name:  "doubled dot leaves an empty segment",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"items..sku"},"elemMatch":"items"}`,
			want:  "invalid mongo path",
		},
		{
			name:  "trailing dot",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"items.sku."}}`,
			want:  "invalid mongo path",
		},
		{
			name:  "leading dot",
			field: `{"name":"sku","type":"string","mapping":{"mongo":".items.sku"}}`,
			want:  "invalid mongo path",
		},
		{
			name:  "segment starting with $ would be read as an operator",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"$where"}}`,
			want:  "invalid mongo path",
		},
		{
			name:  "operator injected mid-path",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"items.$gt"},"elemMatch":"items"}`,
			want:  "invalid mongo path",
		},
		{
			name:  "elemMatch itself malformed",
			field: `{"name":"sku","type":"string","mapping":{"mongo":"items.sku"},"elemMatch":"items."}`,
			want:  "invalid elemMatch path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"entity":"Order","model":{},"fields":[` + tc.field + `]}`
			_, err := ParseConfig([]byte(raw))
			if err == nil {
				t.Fatalf("config loaded but should have been rejected: %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestConfigAcceptsGoodNestedPaths is the matching happy path, so the rejection
// rules above cannot be satisfied by simply refusing everything.
func TestConfigAcceptsGoodNestedPaths(t *testing.T) {
	fields := []string{
		`{"name":"city","type":"string","mapping":{"mongo":"address.city"}}`,
		`{"name":"sku","type":"string","mapping":{"mongo":"items.sku"},"elemMatch":"items"}`,
		`{"name":"w","type":"number","mapping":{"mongo":"items.dims.w"},"elemMatch":"items"}`,
		`{"name":"deep","type":"number","mapping":{"mongo":"a.b.c.d"},"elemMatch":"a.b"}`,
		`{"name":"plain","type":"string"}`,
	}
	for _, f := range fields {
		raw := `{"entity":"Order","model":{},"fields":[` + f + `]}`
		if _, err := ParseConfig([]byte(raw)); err != nil {
			t.Errorf("valid field rejected: %s\n  %v", f, err)
		}
	}
}

// TestMongoElemMatchRelativePathSplit pins the accessor the generator relies on,
// including the nested-under-nested case (elemMatch "a.b" inside path "a.b.c.d").
func TestMongoElemMatchRelativePathSplit(t *testing.T) {
	c := mustParse(t, `{"entity":"Order","model":{},"fields":[
	  {"name":"deep","type":"number","mapping":{"mongo":"a.b.c.d"},"elemMatch":"a.b"},
	  {"name":"plain","type":"string","mapping":{"mongo":"status"}}
	]}`)

	arr, rel, ok := c.MongoElemMatch("deep")
	if !ok || arr != "a.b" || rel != "c.d" {
		t.Errorf("MongoElemMatch(deep) = (%q,%q,%v), want (a.b, c.d, true)", arr, rel, ok)
	}
	if _, _, ok := c.MongoElemMatch("plain"); ok {
		t.Error("a field without elemMatch reported as nested")
	}
	if _, _, ok := c.MongoElemMatch("nosuchfield"); ok {
		t.Error("an unregistered field reported as nested")
	}
}
