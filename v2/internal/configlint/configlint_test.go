package configlint

import (
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
)

// Phase 8's warning channel. The cases are the ones decisions 016 and 019 §4
// named, plus each exemption tested in both directions: the exempt shape is
// silent, and the same shape without the exemption's reason warns.

func load(t *testing.T, name string) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s must still load — warnings never reject a config: %v", name, err)
	}
	return config.NewSchemaRegistry(cfg)
}

// decode builds a registry from inline JSON, for shapes no fixture has.
func decode(t *testing.T, js string) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Decode(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("the inline config must be valid, so only warnings are under test: %v", err)
	}
	return config.NewSchemaRegistry(cfg)
}

// find returns the warnings of a rule whose revealed text contains every
// fragment.
func find(ws []config.Violation, rule config.Rule, fragments ...string) []config.Violation {
	var out []config.Violation
	for _, w := range ws {
		if w.Rule != rule {
			continue
		}
		all := true
		for _, f := range fragments {
			if !strings.Contains(w.Path+" "+w.DetailRevealed(), f) {
				all = false
				break
			}
		}
		if all {
			out = append(out, w)
		}
	}
	return out
}

// Decision 019 §4's three examples, each named with both kinds and both
// config locations.
func TestTheCollisionsDecision019Named(t *testing.T) {
	ws := Check(load(t, "commerce.json"))
	for _, c := range []struct {
		name      string
		fragments []string
	}{
		{"product: entity and relationship (S05)", []string{`"product" is configured on the entity product`, "relationship order to product", "entities[2] (product).name", "relationships[1] (order_product).synonyms"}},
		{"highest: aggregation and pattern (O06)", []string{`"highest" is configured on the aggregation max`, "pattern top_n", "aggregations.max", "queryPatterns[0] (top_n).phrases"}},
		{"placed: relationship and field (D05)", []string{`"placed" on the relationship customer to order`, "field order placed at", "relationships[0] (customer_orders).synonyms", "fields[10] (order.placed_at).name"}},
	} {
		if got := find(ws, config.RuleSharedAcrossKinds, c.fragments...); len(got) != 1 {
			t.Errorf("%s: %d matching warnings, want 1", c.name, len(got))
		}
	}
}

// Decision 016: "one of" contains a word Phase 3 always parses as a number.
// "none of" does not ("none" is not a number word), so it is not reported —
// the check is Phase 3's parser, not a word list.
func TestNumberWordPhrasesNeverForm(t *testing.T) {
	ws := Check(load(t, "commerce.json"))
	if got := find(ws, config.RulePhraseNeverForms, "operators.in", `"one of"`, `contains "one"`, "as a number"); len(got) != 1 {
		t.Errorf(`"one of": %d warnings, want 1`, len(got))
	}
	if got := find(ws, config.RulePhraseNeverForms, `"none of"`); len(got) != 0 {
		t.Errorf(`"none of" reported: %v`, got)
	}
}

// The same check finds date phrases: "last year" on the recent pattern is
// always extracted as a date, never as text.
func TestDatePhrasesNeverForm(t *testing.T) {
	ws := Check(load(t, "commerce.json"))
	if got := find(ws, config.RulePhraseNeverForms, "queryPatterns[3] (recent).phrases", `"last year"`, "as a date"); len(got) != 1 {
		t.Errorf(`"last year": %d warnings, want 1`, len(got))
	}
}

// "how many" configures the count aggregation and the count pattern, which
// ranking treats as one reading (ranking.Agrees): not a collision. "highest"
// on max and top_n is the same SHAPE without the agreement, and is reported
// (above).
func TestAgreeingReadingsAreNotReported(t *testing.T) {
	ws := Check(load(t, "commerce.json"))
	for _, p := range []string{`"how many"`, `"number of"`} {
		if got := find(ws, config.RuleSharedAcrossKinds, p); len(got) != 0 {
			t.Errorf("%s reported though its readings agree: %v", p, got)
		}
	}
}

// "order" inside placed_at's "order date" names the field's own entity, and
// "customer" inside the join key order.customer_id's "customer id" names the
// entity it references: both are how configs are written, and silent.
// "placed" inside "placed at" names neither, and is reported (above).
func TestOwnerQualifiedPhrasesAreNotReported(t *testing.T) {
	ws := Check(load(t, "commerce.json"))
	for _, c := range [][]string{
		{`"order" on the entity order`, `"order date"`},
		{`"customer" on the entity customer`, `"customer id"`},
		{`"product" on the relationship order to product`, `"product id"`},
	} {
		if got := find(ws, config.RuleSharedAcrossKinds, c...); len(got) != 0 {
			t.Errorf("%v reported: %v", c, got)
		}
	}
}

const valueConfig = `{
  "entities": [{"name": "member", "synonyms": ["premium"]}],
  "fields": [
    {"entity": "member", "name": "tier", "type": "enum", "enumValues": ["premium", "basic"], "valueAliases": {"two day": "basic"}},
    {"entity": "member", "name": "country", "type": "string", "valueAliases": {"United States": "US"}},
    {"entity": "member", "name": "status", "type": "string", "synonyms": ["state"]}
  ],
  "operators": {"eq": ["is"]}
}`

// Config VALUES in a warning are redacted by default and shown only in the
// revealed (--debug) rendering. Two shapes:
//   - "premium" is an enum value AND the entity's synonym. The phrase is the
//     same text on both sides, so it is withheld even though the entity side
//     is schema structure. The first draft quoted it from that side; the
//     leak was found while writing this test, and the Phase 8 mutation set
//     reverts the fix to prove this test catches it.
//   - the alias "two day" contains "two", a number word. ("one-off" does not:
//     Phase 3 keeps a hyphenated word whole — the check follows Phase 3.)
func TestValuesInWarningsAreRedacted(t *testing.T) {
	ws := Check(decode(t, valueConfig))
	shared := find(ws, config.RuleSharedAcrossKinds, `"premium"`)
	never := find(ws, config.RulePhraseNeverForms, `"two day"`)
	if len(shared) != 1 || len(never) != 1 {
		t.Fatalf("premium: %d warnings, two day: %d; want 1 and 1\n%v", len(shared), len(never), ws)
	}
	for _, w := range append(shared, never...) {
		if strings.Contains(w.Detail(), "premium") || strings.Contains(w.Detail(), "two day") || strings.Contains(w.Path+w.Detail(), `"two"`) {
			t.Errorf("default rendering leaks a value: %s", w.String())
		}
		if !w.HasRedactedValues() {
			t.Errorf("warning does not report that it withheld something: %s", w.String())
		}
	}
	// Structure survives redaction: the field the value belongs to is named.
	if !strings.Contains(shared[0].Detail(), "value of field member.tier") {
		t.Errorf("redacted warning lost its structure: %s", shared[0].Detail())
	}
}

// A word inside a value is not a collision: a value is accepted only when the
// sentence states all of it (019 §1). "state" (status's synonym) inside the
// alias "United States" is silent; the same word inside a SCHEMA phrase of
// another kind warns.
func TestWordsInsideValuesAreNotReported(t *testing.T) {
	if got := find(Check(decode(t, valueConfig)), config.RuleSharedAcrossKinds, `"state"`); len(got) != 0 {
		t.Errorf("a word inside a value was reported: %v", got)
	}
	schemaShape := strings.Replace(valueConfig, `"operators": {"eq": ["is"]}`,
		`"operators": {"eq": ["is"]}, "aggregations": {"count": ["united state"]}`, 1)
	if got := find(Check(decode(t, schemaShape)), config.RuleSharedAcrossKinds, `"state" on the field`); len(got) != 1 {
		t.Errorf("the same word inside a schema phrase: %d warnings, want 1", len(got))
	}
}

// Same config, same warnings, same order: the output is byte-identical.
func TestWarningsAreDeterministic(t *testing.T) {
	reg := load(t, "commerce.json")
	render := func() string {
		var b strings.Builder
		for _, w := range Check(reg) {
			b.WriteString(w.StringRevealed() + "\n")
		}
		return b.String()
	}
	first := render()
	for i := 0; i < 5; i++ {
		if render() != first {
			t.Fatal("warnings changed between runs")
		}
	}
}

// A config with no collisions reports nothing: the channel is quiet by
// default, not a list of trivia.
func TestCleanConfigHasNoWarnings(t *testing.T) {
	if ws := Check(load(t, "customers.json")); len(ws) != 0 {
		t.Errorf("customers.json: %v", ws)
	}
}
