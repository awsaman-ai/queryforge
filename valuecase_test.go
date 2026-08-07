package queryforge

// Tests for the per-field value-case rule (Field.ValueCase).
//
// The rule has one job — the string that reaches the database carries the
// case the column actually stores — and two promises around it: nothing above
// the generator changes (the AST, the enum domain and validation all keep the
// config's own spelling), and nothing that is not a string is touched. The
// tests below pin both halves, on both backends, plus the load-time rejections
// that stop a silently-inert setting from shipping.

import (
	"encoding/json"
	"strings"
	"testing"
)

// caseConfigJSON registers the same logical fields twice — once plain, once
// with a case rule — so every assertion can compare the two side by side and
// attribute a difference to the rule rather than to the operator.
const caseConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["shipped","cancelled"],"valueCase":"upper",
     "operators":["equals","notEquals","in","notIn"]},
    {"name":"statusPlain","type":"enum","values":["shipped","cancelled"],
     "operators":["equals","in"],"mapping":{"sql":"status2","mongo":"status2"}},
    {"name":"code","type":"string","valueCase":"upper",
     "operators":["equals","contains","startsWith","endsWith","regex","between","in"]},
    {"name":"email","type":"string","valueCase":"lower",
     "operators":["equals","contains","startsWith"]},
    {"name":"tags","type":"array","itemType":"string","valueCase":"upper",
     "operators":["contains","containsAny","containsAll","in"]},
    {"name":"total","type":"number","operators":["equals","gt","between"]},
    {"name":"createdAt","type":"date","operators":["equals","after","between"]},
    {"name":"note","type":"string","operators":["equals","contains"]}
  ],
  "defaults":{"limit":50}
}`

func caseConfig(t *testing.T) *Config { return mustParse(t, caseConfigJSON) }

// argsOf runs the SQL generator and returns just the bound arguments, which is
// where every user-supplied value lands in that backend.
func argsOf(t *testing.T, c *Config, cmp *Condition) []any {
	t.Helper()
	return genSQL(t, c, single(cmp)).Args
}

// mongoFilterOf renders the Mongo filter document as key-sorted JSON, so a test
// can pin the exact value in one readable string.
func mongoFilterOf(t *testing.T, c *Config, cmp *Condition) string {
	t.Helper()
	b, err := json.Marshal(genMongo(t, c, single(cmp)).Filter)
	if err != nil {
		t.Fatalf("marshal filter: %v", err)
	}
	return string(b)
}

// TestValueCaseAppliesToBothBackends is the happy path: the same AST, spelled
// the way the config's enum domain spells it, must reach SQL and Mongo in the
// physical case the field declared.
func TestValueCaseAppliesToBothBackends(t *testing.T) {
	c := caseConfig(t)

	cases := []struct {
		name  string
		cmp   *Condition
		arg   any    // expected single bound SQL argument
		mongo string // expected Mongo filter document
	}{
		{"enum equals is upper-cased", comp("status", OpEquals, vEnum("shipped")),
			"SHIPPED", `{"status":"SHIPPED"}`},
		{"string equals is upper-cased", comp("code", OpEquals, vStr("ab-12")),
			"AB-12", `{"code":"AB-12"}`},
		{"lower rule folds down", comp("email", OpEquals, vStr("Sam@Example.COM")),
			"sam@example.com", `{"email":"sam@example.com"}`},
		{"notEquals is covered too", comp("status", OpNotEquals, vEnum("cancelled")),
			"CANCELLED", `{"status":{"$exists":true,"$ne":"CANCELLED"}}`},
		{"no rule leaves the value alone", comp("statusPlain", OpEquals, vEnum("shipped")),
			"shipped", `{"status2":"shipped"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := argsOf(t, c, tc.cmp)
			if len(args) != 1 || args[0] != tc.arg {
				t.Errorf("sql args = %#v, want [%#v]", args, tc.arg)
			}
			if got := mongoFilterOf(t, c, tc.cmp); got != tc.mongo {
				t.Errorf("mongo filter = %s, want %s", got, tc.mongo)
			}
		})
	}
}

// TestValueCaseAppliesToEveryStringOperator walks the operators that carry
// strings. Each is a separate code path in both generators — a scalar, a list,
// a LIKE pattern, an array literal — and an operator missed in one of them
// would produce a query that silently matches nothing.
func TestValueCaseAppliesToEveryStringOperator(t *testing.T) {
	c := caseConfig(t)

	cases := []struct {
		name  string
		cmp   *Condition
		args  []any
		mongo string
	}{
		{"in list", comp("status", OpIn, vArr("shipped", "cancelled")),
			[]any{"SHIPPED", "CANCELLED"}, `{"status":{"$in":["SHIPPED","CANCELLED"]}}`},
		{"notIn list", comp("status", OpNotIn, vArr("shipped")),
			[]any{"SHIPPED"}, `{"status":{"$exists":true,"$nin":["SHIPPED"]}}`},
		{"between endpoints", comp("code", OpBetween, vArr("aa", "zz")),
			[]any{"AA", "ZZ"}, `{"code":{"$gte":"AA","$lte":"ZZ"}}`},
		// LIKE and $regex build a pattern around the value: the fold must happen
		// to the value only, leaving the wildcards and anchors where they were.
		{"startsWith pattern", comp("code", OpStartsWith, vStr("ab")),
			[]any{"AB%"}, `{"code":{"$regex":"^AB"}}`},
		{"endsWith pattern", comp("code", OpEndsWith, vStr("ab")),
			[]any{"%AB"}, `{"code":{"$regex":"AB$"}}`},
		{"contains on a string", comp("email", OpContains, vStr("EXAMPLE")),
			[]any{"%example%"}, `{"email":{"$options":"i","$regex":"example"}}`},
		// Array fields fold their elements, whether one or many.
		{"contains on an array", comp("tags", OpContains, vStr("vip")),
			[]any{"VIP"}, `{"tags":"VIP"}`},
		{"containsAny", comp("tags", OpContainsAny, vArr("vip", "gold")),
			[]any{"VIP", "GOLD"}, `{"tags":{"$in":["VIP","GOLD"]}}`},
		{"containsAll", comp("tags", OpContainsAll, vArr("vip", "gold")),
			[]any{"VIP", "GOLD"}, `{"tags":{"$all":["VIP","GOLD"]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := argsOf(t, c, tc.cmp)
			if len(args) != len(tc.args) {
				t.Fatalf("sql args = %#v, want %#v", args, tc.args)
			}
			for i := range args {
				if args[i] != tc.args[i] {
					t.Errorf("sql arg %d = %#v, want %#v", i, args[i], tc.args[i])
				}
			}
			if got := mongoFilterOf(t, c, tc.cmp); got != tc.mongo {
				t.Errorf("mongo filter = %s, want %s", got, tc.mongo)
			}
		})
	}
}

// TestValueCaseNeverTouchesARegexPattern is the deliberate exemption. A pattern
// is not a word: folding it rewrites its escapes, and "\d" (a digit) becoming
// "\D" (not a digit) would invert the predicate while still compiling cleanly.
func TestValueCaseNeverTouchesARegexPattern(t *testing.T) {
	c := caseConfig(t)
	pattern := `^ab-\d+$` // upper-casing this would flip \d to \D

	cmp := comp("code", OpRegex, vStr(pattern))
	if args := argsOf(t, c, cmp); len(args) != 1 || args[0] != pattern {
		t.Errorf("sql args = %#v, want the pattern verbatim [%q]", args, pattern)
	}
	want := `{"code":{"$regex":"^ab-\\d+$"}}`
	if got := mongoFilterOf(t, c, cmp); got != want {
		t.Errorf("mongo filter = %s, want %s", got, want)
	}
}

// TestValueCaseLeavesNonStringsAlone guards the other side of the rule. A
// number, a boolean or a date must round-trip untouched even when it sits in
// the same query as a cased field — including the date strings a between/in
// list carries, which are text right up to the moment they are parsed.
func TestValueCaseLeavesNonStringsAlone(t *testing.T) {
	c := caseConfig(t)

	q := NewQuery("Order")
	q.Filter = and(
		comp("status", OpEquals, vEnum("shipped")), // the cased field
		comp("total", OpGt, vNum(99.5)),
		comp("createdAt", OpAfter, vStr("2026-01-02")),
	)

	args := genSQL(t, c, q).Args
	want := []any{"SHIPPED", 99.5, "2026-01-02"}
	if len(args) != len(want) {
		t.Fatalf("sql args = %#v, want %#v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("sql arg %d = %#v, want %#v", i, args[i], want[i])
		}
	}
}

// TestValueCaseDoesNotMutateTheAST pins the library's standing contract that
// compiling a query never writes through to the caller's AST. The array path is
// where that is easy to get wrong: AsSlice hands back the payload itself, so
// folding in place would leave the caller holding a rewritten query — and a
// second Generate against another backend would then see the wrong values.
func TestValueCaseDoesNotMutateTheAST(t *testing.T) {
	c := caseConfig(t)

	scalar := vEnum("shipped")
	list := vArr("vip", "gold")
	q := NewQuery("Order")
	q.Filter = and(comp("status", OpEquals, scalar), comp("tags", OpContainsAny, list))

	// Parenthesised: a composite literal cannot start an if-statement header.
	if _, err := (SQLGenerator{}).Generate(q, c, GenOptions{Now: fixedNow}); err != nil {
		t.Fatalf("sql generate: %v", err)
	}
	if _, err := (MongoGenerator{}).Generate(q, c, GenOptions{Now: fixedNow}); err != nil {
		t.Fatalf("mongo generate: %v", err)
	}

	if s, _ := scalar.AsString(); s != "shipped" {
		t.Errorf("scalar value was mutated to %q; the AST must survive generation unchanged", s)
	}
	elems, _ := list.AsSlice()
	for i, want := range []string{"vip", "gold"} {
		if elems[i] != want {
			t.Errorf("array element %d was mutated to %#v, want %q", i, elems[i], want)
		}
	}
}

// TestValueCaseIsInvisibleToValidation pins the decision that the config's
// `values` stay the single spoken vocabulary: the model emits the spelling the
// config lists, validation checks that spelling verbatim, and only the built
// query carries the physical case. The upper-case form is therefore *not* a
// member of the domain, however the query eventually renders.
func TestValueCaseIsInvisibleToValidation(t *testing.T) {
	c := caseConfig(t)

	if err := Validate(single(comp("status", OpEquals, vEnum("shipped"))), c); err != nil {
		t.Fatalf("the config's own spelling must validate, got %v", err)
	}
	if err := Validate(single(comp("status", OpEquals, vEnum("SHIPPED"))), c); err == nil {
		t.Fatal("a value outside the declared domain must be rejected, even when it is the case the query will use")
	}
}

// TestValueCaseAppliesInsideElemMatch checks the rule survives the nested-Mongo
// path, which re-keys the predicate relative to an array element and is the one
// place a value could reach the filter through a different function.
func TestValueCaseAppliesInsideElemMatch(t *testing.T) {
	c := mustParse(t, `{
      "entity":"Order","model":{},
      "backends":{"mongo":{"collection":"orders"}},
      "fields":[
        {"name":"itemSku","type":"string","valueCase":"upper",
         "operators":["equals"],"mapping":{"mongo":"items.sku"},"elemMatch":"items"}
      ]}`)

	got := mongoFilterOf(t, c, comp("itemSku", OpEquals, vStr("ab-12")))
	want := `{"items":{"$elemMatch":{"sku":"AB-12"}}}`
	if got != want {
		t.Errorf("mongo filter = %s, want %s", got, want)
	}
}

// TestValueCaseAppliesToInjectedScope covers the application-supplied filters.
// They are merged into the tree after validation, so they never pass through
// the model or the validator — the generator is the only place a case rule can
// reach them, and a tenant column stored upper-case needs it just as much.
func TestValueCaseAppliesToInjectedScope(t *testing.T) {
	c := caseConfig(t)

	q := NewQuery("Order")
	q.Filter = comp("total", OpGt, vNum(10))
	filters, err := normalizeScope(Scope{"code": "ab-12"}, c)
	if err != nil {
		t.Fatalf("normalize scope: %v", err)
	}

	got := mongoFilterOf2(t, c, applyScope(q, filters))
	if !strings.Contains(got, `"code":"AB-12"`) {
		t.Errorf("scope filter = %s, want the code forced to upper case", got)
	}
}

// mongoFilterOf2 is mongoFilterOf for a whole query rather than one predicate.
func mongoFilterOf2(t *testing.T, c *Config, q *Query) string {
	t.Helper()
	b, err := json.Marshal(genMongo(t, c, q).Filter)
	if err != nil {
		t.Fatalf("marshal filter: %v", err)
	}
	return string(b)
}

// TestValueCaseOddValues pushes strings the rule was not designed around. None
// of these should error or change length-sensitive behaviour: the fold is a
// plain Unicode case mapping, and a value with no case must come back
// byte-identical rather than mangled.
func TestValueCaseOddValues(t *testing.T) {
	c := caseConfig(t)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty string", "", ""},
		{"digits and punctuation have no case", "12-34_/", "12-34_/"},
		{"already upper", "AB-12", "AB-12"},
		{"non-latin script has no case", "订单", "订单"},
		{"accented latin folds", "café", "CAFÉ"},
		// ß has no single-rune upper case, so Go leaves it alone. Pinned because
		// a future switch to a locale-aware folder would silently change the
		// stored value's length.
		{"eszett is left alone", "straße", "STRAßE"},
		{"a quote is not escaped or altered", "o'brien", "O'BRIEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := argsOf(t, c, comp("code", OpEquals, vStr(tc.in)))
			if len(args) != 1 || args[0] != tc.want {
				t.Errorf("sql args = %#v, want [%q]", args, tc.want)
			}
		})
	}
}

// TestValueCaseIgnoresNonStringElements guards the array path against a mixed
// list. Validation rejects one before it reaches a generator, but applyElems is
// reached by scope injection too, and a number quietly turned into a string
// would produce a filter that matches nothing.
func TestValueCaseIgnoresNonStringElements(t *testing.T) {
	c := caseConfig(t)

	got := mongoFilterOf(t, c, comp("tags", OpContainsAny, vArr("vip", 7, true, nil)))
	want := `{"tags":{"$in":["VIP",7,true,null]}}`
	if got != want {
		t.Errorf("mongo filter = %s, want %s", got, want)
	}
}

// TestValueCaseConfigRejections is the adversarial half of the load path. Both
// mistakes here are invisible at query time — the setting simply does nothing —
// so the loader has to refuse them rather than shrug.
func TestValueCaseConfigRejections(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want string // substring the error must name, so the message stays useful
	}{
		{"misspelt setting", `{"name":"status","type":"string","valueCase":"uppercase"}`, "invalid valueCase"},
		{"wrong case for the setting itself", `{"name":"status","type":"string","valueCase":"UPPER"}`, "invalid valueCase"},
		{"on a number", `{"name":"total","type":"number","valueCase":"upper"}`, "not strings"},
		{"on a date", `{"name":"createdAt","type":"date","valueCase":"lower"}`, "not strings"},
		{"on a boolean", `{"name":"refunded","type":"boolean","valueCase":"upper"}`, "not strings"},
		{"on an array of numbers", `{"name":"sizes","type":"array","itemType":"number","valueCase":"upper"}`, "not strings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(`{"entity":"Order","model":{},"fields":[` + tc.spec + `]}`))
			if err == nil {
				t.Fatal("config loaded, but the setting could never take effect")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestValueCaseConfigAcceptances is the matching allow-list: every field shape
// whose values are text must take the rule, including an array that leaves
// itemType unset (it defaults to string, so it does carry letters).
func TestValueCaseConfigAcceptances(t *testing.T) {
	cases := []string{
		`{"name":"a","type":"string","valueCase":"upper"}`,
		`{"name":"a","type":"string","valueCase":"lower"}`,
		`{"name":"a","type":"enum","values":["x"],"valueCase":"upper"}`,
		`{"name":"a","type":"array","itemType":"string","valueCase":"upper"}`,
		`{"name":"a","type":"array","itemType":"enum","values":["x"],"valueCase":"upper"}`,
		`{"name":"a","type":"array","valueCase":"upper"}`, // itemType unset = string
		`{"name":"a","type":"number"}`,                    // no rule on a non-string field is fine
	}
	for _, spec := range cases {
		t.Run(spec, func(t *testing.T) {
			if _, err := ParseConfig([]byte(`{"entity":"Order","model":{},"fields":[` + spec + `]}`)); err != nil {
				t.Fatalf("config should load: %v", err)
			}
		})
	}
}

// TestValueCaseRoundTripsThroughJSON checks the key survives a config being
// read and written back — the builder's import/export path, and anything else
// that re-serializes a Config.
func TestValueCaseRoundTripsThroughJSON(t *testing.T) {
	c := caseConfig(t)
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := ParseConfig(out)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	f, ok := back.FieldByName("status")
	if !ok {
		t.Fatal("field status missing after round trip")
	}
	if f.ValueCase != CaseUpper {
		t.Errorf("valueCase = %q after round trip, want %q", f.ValueCase, CaseUpper)
	}
	// A field that never declared one must not gain the key on the way out.
	if strings.Count(string(out), `"valueCase"`) != 4 {
		t.Errorf("valueCase appears %d times in the serialized config, want 4 (only the fields that declare it)",
			strings.Count(string(out), `"valueCase"`))
	}
}
