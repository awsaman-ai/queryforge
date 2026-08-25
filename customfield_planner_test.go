package queryforge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// --- Tier 1 prompt-injection tests: what the model actually sees.
//
// Business rule under test: the metadata is genuinely useful context for the
// model (it must appear when set), it must never replace the field's logical
// name as the thing the model is told to emit, it must never leak for a
// hidden field, and — the whole "invisible unless used" promise — an ordinary
// config's prompt must be byte-for-byte unchanged by this feature existing.

var fixedPromptTime = time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)

// TestPromptUnchangedWhenTier1Unused is the regression guard for the central
// design promise: a config that sets none of the new keys produces the exact
// same prompt line it would have before this feature existed — no label=,
// desc=, hint=, or reminder sentence anywhere.
func TestPromptUnchangedWhenTier1Unused(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "fields":[{"name":"status","type":"enum","values":["PLACED","DELIVERED"],"operators":["equals"]}]
	}`)
	pl := NewPlanner(c, &StubProvider{})
	prompt := pl.SystemPrompt(fixedPromptTime)

	want := "- status (enum) values=[PLACED,DELIVERED] operators=[equals]\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt line changed for a field using none of the Tier-1 keys:\ngot line missing %q\n---\n%s", want, prompt)
	}
	for _, unwanted := range []string{"label=", "desc=", "hint=", "reference the field by the name"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("unused feature leaked into prompt: found %q\n---\n%s", unwanted, prompt)
		}
	}
}

// TestPromptIncludesCustomFieldContext is the positive case: a customField's
// full context block (label, description, valueHint) must reach the prompt,
// since that context is the entire reason the field can be shipped at all.
func TestPromptIncludesCustomFieldContext(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "fields":[
	    {"name":"txt01","type":"string","customField":true,
	     "displayName":"Passport Country","description":"ISO country of issuance",
	     "valueHint":"Free-text notes entered by agents about travel documents."}
	  ]
	}`)
	pl := NewPlanner(c, &StubProvider{})
	prompt := pl.SystemPrompt(fixedPromptTime)

	for _, want := range []string{
		`label="Passport Country"`,
		`desc="ISO country of issuance"`,
		`hint="Free-text notes entered by agents about travel documents."`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
}

// TestPromptFieldNameStaysThePrimaryToken proves the label never displaces
// the logical name: the model must still be told to reference the field as
// "txt01", not "Passport Country" — the AST vocabulary is unaffected by this
// feature. A regression here would silently break every custom field, since
// the model would emit an unknown field name and every request would refuse.
func TestPromptFieldNameStaysThePrimaryToken(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "fields":[
	    {"name":"txt01","type":"string","customField":true,
	     "displayName":"Passport Country","description":"ISO country of issuance",
	     "valueHint":"agent notes"}
	  ]
	}`)
	pl := NewPlanner(c, &StubProvider{})
	prompt := pl.SystemPrompt(fixedPromptTime)

	if !strings.Contains(prompt, "- txt01 (string)") {
		t.Errorf("field's logical name is no longer the leading, unambiguous token:\n%s", prompt)
	}
	if !strings.Contains(prompt, "reference the field by the name shown first on its line") {
		t.Errorf("prompt should explicitly warn the model not to emit the label:\n%s", prompt)
	}
}

// TestPromptReminderOnlyAppearsWhenALabelIsSet: the "reference by name, not
// label" sentence costs every future request a few tokens, so it must only
// appear when a field actually sets a displayName — a customField that only
// sets description/valueHint (no displayName) has no label to be confused by,
// and an ordinary config never using the feature must not pay for it either.
func TestPromptReminderOnlyAppearsWhenALabelIsSet(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "fields":[
	    {"name":"txt01","type":"string","customField":true,
	     "description":"ISO country of issuance","valueHint":"agent notes"}
	  ]
	}`)
	pl := NewPlanner(c, &StubProvider{})
	prompt := pl.SystemPrompt(fixedPromptTime)

	if strings.Contains(prompt, "reference the field by the name shown first on its line") {
		t.Errorf("reminder should not appear when no field sets a displayName:\n%s", prompt)
	}
}

// TestPromptNeverLeaksHiddenCustomFieldMetadata: queryable:false already means
// a field must never reach the model. A customField flag (and its now-mandatory
// description/valueHint) must not create a second, accidental path for a
// hidden field's data to leak into the prompt.
func TestPromptNeverLeaksHiddenCustomFieldMetadata(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "fields":[
	    {"name":"visible","type":"string"},
	    {"name":"secretCustom","type":"string","queryable":false,"customField":true,
	     "description":"internal-only note about the customer","valueHint":"never shown to users"}
	  ]
	}`)
	pl := NewPlanner(c, &StubProvider{})
	prompt := pl.SystemPrompt(fixedPromptTime)

	for _, leaked := range []string{"secretCustom", "internal-only note", "never shown to users"} {
		if strings.Contains(prompt, leaked) {
			t.Errorf("hidden customField leaked into prompt: found %q\n---\n%s", leaked, prompt)
		}
	}
}

// TestCustomFieldRoundTripsThroughPlanAndValidate is the end-to-end business
// check: a model that (correctly) answers using the field's logical name —
// exactly as the prompt instructs — produces an AST that Validate accepts.
// This is what actually proves the label/description addition does not break
// the pipeline, as opposed to merely inspecting the prompt string.
func TestCustomFieldRoundTripsThroughPlanAndValidate(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "fields":[
	    {"name":"txt01","type":"string","customField":true,
	     "displayName":"Passport Country","description":"ISO country of issuance",
	     "valueHint":"Free-text notes entered by agents about travel documents."}
	  ]
	}`)
	canned := `{"entity":"Order","filter":{"type":"comparison","field":"txt01","operator":"contains","value":{"kind":"string","v":"France"}}}`
	pl := NewPlanner(c, &StubProvider{Response: canned})

	q, _, err := pl.Plan(context.Background(), "passport country mentions France", RepairHint{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := Validate(q, c); err != nil {
		t.Fatalf("Validate rejected an AST referencing the field by its logical name: %v", err)
	}
}
