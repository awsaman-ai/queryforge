package queryforge

import (
	"strings"
	"testing"
)

// ciConfigJSON declares one field of every shape relevant to caseInsensitive:
// a string field with it on, a string field without it (regression control),
// and an enum field (to prove the flag is rejected there at load).
const ciConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mysql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"color","type":"string","caseInsensitive":true,
     "operators":["equals","notEquals","in","notIn","contains","startsWith","endsWith"],
     "mapping":{"sql":"color","mongo":"color"}},
    {"name":"note","type":"string",
     "operators":["equals","contains"],
     "mapping":{"sql":"note","mongo":"note"}},
    {"name":"status","type":"enum","values":["PLACED","DELIVERED"],
     "mapping":{"sql":"status","mongo":"status"}}
  ],
  "defaults":{"limit":50}
}`

func ciConfig(t *testing.T) *Config { return mustParse(t, ciConfigJSON) }

// --- config validation -------------------------------------------------

func TestCaseInsensitiveRejectedOnNonStringField(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "entity":"Order","model":{"model":"x"},
	  "fields":[{"name":"status","type":"enum","values":["A","B"],"caseInsensitive":true}]
	}`))
	if err == nil {
		t.Fatal("caseInsensitive on an enum field was accepted")
	}
	if !strings.Contains(err.Error(), "applies to string fields only") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCaseInsensitiveRejectedWithValueCase(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "entity":"Order","model":{"model":"x"},
	  "fields":[{"name":"color","type":"string","valueCase":"upper","caseInsensitive":true}]
	}`))
	if err == nil {
		t.Fatal("caseInsensitive combined with valueCase was accepted")
	}
	if !strings.Contains(err.Error(), "cannot both apply") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCaseInsensitiveAcceptedOnStringField(t *testing.T) {
	c := ciConfig(t)
	f, ok := c.FieldByName("color")
	if !ok {
		t.Fatal("field color missing")
	}
	if !f.CaseInsensitive {
		t.Error("color.caseInsensitive = false, want true")
	}
}

// --- SQL: Postgres -------------------------------------------------------

func TestCaseInsensitivePostgresEquals(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpEquals, vStr("Black"))
	r := genSQL(t, c, q)
	// Only the column is folded in the SQL text; the argument is folded at
	// bind time (checked below) rather than wrapped in the placeholder.
	if !strings.Contains(r.SQL, "LOWER(color) = $1") {
		t.Errorf("sql = %q, want LOWER(color) = $1", r.SQL)
	}
	if len(r.Args) != 1 || r.Args[0] != "black" {
		t.Errorf("args = %v, want [\"black\"] (folded at bind time)", r.Args)
	}
}

func TestCaseInsensitivePostgresNotEquals(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpNotEquals, vStr("Black"))
	r := genSQL(t, c, q)
	if !strings.Contains(r.SQL, "LOWER(color) <> $1") {
		t.Errorf("sql = %q, want LOWER(color) <> $1", r.SQL)
	}
}

func TestCaseInsensitivePostgresInList(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpIn, vArr("Black", "White"))
	r := genSQL(t, c, q)
	if !strings.Contains(r.SQL, "LOWER(color) IN ($1, $2)") {
		t.Errorf("sql = %q, want LOWER(color) IN ($1, $2)", r.SQL)
	}
	if r.Args[0] != "black" || r.Args[1] != "white" {
		t.Errorf("args = %v, want folded to lower case", r.Args)
	}
}

func TestCaseInsensitivePostgresContainsUsesILIKE(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpContains, vStr("Bla"))
	r := genSQL(t, c, q)
	// ILIKE needs no LOWER() wrapping on the column — Postgres's own
	// case-insensitive match does the folding.
	if !strings.Contains(r.SQL, "color ILIKE $1") {
		t.Errorf("sql = %q, want \"color ILIKE $1\"", r.SQL)
	}
	if r.Args[0] != "%Bla%" {
		t.Errorf("args = %v, want the pattern unfolded — ILIKE folds it, not the argument", r.Args)
	}
}

func TestCaseInsensitivePostgresStartsWithEndsWith(t *testing.T) {
	c := ciConfig(t)

	q := NewQuery("Order")
	q.Filter = comp("color", OpStartsWith, vStr("Bla"))
	r := genSQL(t, c, q)
	if !strings.Contains(r.SQL, "color ILIKE $1") || r.Args[0] != "Bla%" {
		t.Errorf("startsWith: sql = %q args = %v", r.SQL, r.Args)
	}

	q2 := NewQuery("Order")
	q2.Filter = comp("color", OpEndsWith, vStr("ack"))
	r2 := genSQL(t, c, q2)
	if !strings.Contains(r2.SQL, "color ILIKE $1") || r2.Args[0] != "%ack" {
		t.Errorf("endsWith: sql = %q args = %v", r2.SQL, r2.Args)
	}
}

// A field that does not set caseInsensitive must compile exactly as before:
// no LOWER(), no ILIKE, the value untouched.
func TestCaseInsensitiveDefaultOffLeavesPostgresUnchanged(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("note", OpEquals, vStr("Fragile"))
	r := genSQL(t, c, q)
	if r.SQL != "SELECT * FROM orders WHERE note = $1 LIMIT 50" {
		t.Errorf("sql = %q, want an untouched equality", r.SQL)
	}
	if r.Args[0] != "Fragile" {
		t.Errorf("args = %v, want the original casing preserved", r.Args)
	}
}

// --- SQL: MySQL ------------------------------------------------------------
// MySQL has no ILIKE, so every case-insensitive operator — including the
// pattern ones — folds through LOWER() on both sides.

func TestCaseInsensitiveMySQLEquals(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpEquals, vStr("Black"))
	r := tGen(t, c, q, "mysql")
	if !strings.Contains(r.SQL, "LOWER(color) = ?") {
		t.Errorf("sql = %q, want LOWER(color) = ?", r.SQL)
	}
	if r.Args[0] != "black" {
		t.Errorf("args = %v, want folded to lower case", r.Args)
	}
}

func TestCaseInsensitiveMySQLContains(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpContains, vStr("Bla"))
	r := tGen(t, c, q, "mysql")
	if !strings.Contains(r.SQL, "LOWER(color) LIKE ?") {
		t.Errorf("sql = %q, want LOWER(color) LIKE ? (no ILIKE on MySQL)", r.SQL)
	}
	if r.Args[0] != "%bla%" {
		t.Errorf("args = %v, want the pattern folded to lower case", r.Args)
	}
}

func TestCaseInsensitiveMySQLNotIn(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpNotIn, vArr("Black", "White"))
	r := tGen(t, c, q, "mysql")
	if !strings.Contains(r.SQL, "LOWER(color) NOT IN (?, ?)") {
		t.Errorf("sql = %q, want LOWER(color) NOT IN (?, ?)", r.SQL)
	}
	if r.Args[0] != "black" || r.Args[1] != "white" {
		t.Errorf("args = %v, want folded to lower case", r.Args)
	}
}

// --- Mongo -------------------------------------------------------------

func TestCaseInsensitiveMongoEquals(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpEquals, vStr("Black"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"color":{"$options":"i","$regex":"^Black$"}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestCaseInsensitiveMongoNotEquals(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpNotEquals, vStr("Black"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"color":{"$exists":true,"$not":{"$options":"i","$regex":"^Black$"}}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestCaseInsensitiveMongoIn(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpIn, vArr("Black", "White"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"$or":[{"color":{"$options":"i","$regex":"^Black$"}},{"color":{"$options":"i","$regex":"^White$"}}]}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestCaseInsensitiveMongoNotIn(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpNotIn, vArr("Black", "White"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"$and":[{"color":{"$not":{"$options":"i","$regex":"^Black$"}}},{"color":{"$not":{"$options":"i","$regex":"^White$"}}},{"color":{"$exists":true}}]}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestCaseInsensitiveMongoStartsWithEndsWith(t *testing.T) {
	c := ciConfig(t)

	q := NewQuery("Order")
	q.Filter = comp("color", OpStartsWith, vStr("Bla"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"color":{"$options":"i","$regex":"^Bla"}}`
	if got != want {
		t.Errorf("startsWith filter = %s, want %s", got, want)
	}

	q2 := NewQuery("Order")
	q2.Filter = comp("color", OpEndsWith, vStr("ack"))
	got2 := filterJSON(t, genMongo(t, c, q2))
	want2 := `{"color":{"$options":"i","$regex":"ack$"}}`
	if got2 != want2 {
		t.Errorf("endsWith filter = %s, want %s", got2, want2)
	}
}

// contains on Mongo has always matched case-insensitively regardless of the
// flag, so caseInsensitive:true must not change its shape.
func TestCaseInsensitiveMongoContainsUnaffected(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpContains, vStr("Bla"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"color":{"$options":"i","$regex":"Bla"}}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// A field that does not set caseInsensitive must compile exactly as before.
func TestCaseInsensitiveDefaultOffLeavesMongoUnchanged(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("note", OpEquals, vStr("Fragile"))
	got := filterJSON(t, genMongo(t, c, q))
	want := `{"note":"Fragile"}`
	if got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// A caseInsensitive `in`/`notIn` operand must be text — the flag only ever
// applies to a string field, so a non-string element is a config/AST mismatch
// that must error rather than silently produce a bad predicate.
func TestCaseInsensitiveMongoInRejectsNonStringElement(t *testing.T) {
	c := ciConfig(t)
	q := NewQuery("Order")
	q.Filter = comp("color", OpIn, vArr("Black", 5.0))
	_, err := MongoGenerator{}.Generate(q, c, GenOptions{Now: fixedNow})
	if err == nil {
		t.Fatal("a non-string element in a caseInsensitive in-list was accepted")
	}
	if !strings.Contains(err.Error(), "requires string values") {
		t.Errorf("unexpected error: %v", err)
	}
}
