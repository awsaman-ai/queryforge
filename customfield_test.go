package queryforge

import (
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/config"
)

// --- Tier 1: LLM-facing field metadata (displayName/description/valueHint)
// and the CustomField flag that makes context mandatory for a field whose
// name does not explain itself.
//
// These tests are written from the BUSINESS rule the feature exists to
// enforce, not from the Go implementation that happens to enforce it: an
// ordinary field (orderNumber, address, tags) must stay exactly as cheap to
// configure as it was before this feature existed, and a field marked
// customField (txt01, customField1) must be impossible to ship without real
// context for the model — that guarantee is the entire point of the checkbox.

// TestOrdinaryFieldNeedsNoMetadata is the backward-compatibility guarantee:
// a field that predates this feature, or simply has a self-explanatory name,
// loads with none of the four new keys present, and none are silently
// defaulted onto it.
func TestOrdinaryFieldNeedsNoMetadata(t *testing.T) {
	c := mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "orderNumber", "type": "string"},
	    {"name": "address", "type": "string"},
	    {"name": "tags", "type": "array", "itemType": "string"}
	  ]
	}`)
	for _, name := range []string{"orderNumber", "address", "tags"} {
		f, ok := c.FieldByName(name)
		if !ok {
			t.Fatalf("field %q not found", name)
		}
		if f.CustomField || f.DisplayName != "" || f.Description != "" || f.ValueHint != "" {
			t.Errorf("field %q should carry no Tier-1 metadata by default, got %+v", name, f)
		}
	}
}

// TestOrdinaryFieldMetadataStaysOptionalAndIndependent: an author may fill in
// just a displayName for a normal field without being forced into providing
// a description too — decorative labels are opt-in and do not cascade into
// requiring the rest, unlike customField.
func TestOrdinaryFieldMetadataStaysOptionalAndIndependent(t *testing.T) {
	c := mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "customerName", "type": "string", "displayName": "Customer"}
	  ]
	}`)
	f, _ := c.FieldByName("customerName")
	if f.DisplayName != "Customer" {
		t.Errorf("displayName not preserved: %+v", f)
	}
	if f.Description != "" {
		t.Errorf("description should stay empty — it is not required just because displayName was set")
	}
}

// TestCustomFieldRequiresDescription is the core business rule: a field
// flagged customField cannot ship without a description, on ANY field type.
// A label alone ("Passport Country") is not context — the whole reason the
// checkbox exists is to stop exactly that from being forgotten.
func TestCustomFieldRequiresDescription(t *testing.T) {
	cases := map[string]string{
		"string, no description at all":   `{"name":"customField1","type":"string","customField":true}`,
		"number, no description":          `{"name":"num01","type":"number","customField":true}`,
		"boolean, no description":         `{"name":"flag01","type":"boolean","customField":true}`,
		"date, no description":            `{"name":"date03","type":"date","customField":true}`,
		"empty string description":        `{"name":"txt01","type":"string","customField":true,"description":""}`,
		"whitespace-only description":     `{"name":"txt01","type":"string","customField":true,"description":"   "}`,
		"displayName alone is not enough": `{"name":"txt01","type":"string","searchable":false,"customField":true,"displayName":"Passport Country"}`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			js := `{"entity":"Order","fields":[` + field + `]}`
			_, err := ParseConfig([]byte(js))
			if err == nil {
				t.Fatalf("expected rejection (missing description on a customField), got none")
			}
			if !strings.Contains(err.Error(), "description") {
				t.Errorf("error should name the missing description, got: %v", err)
			}
		})
	}
}

// TestCustomFieldSearchableStringRequiresValueHint: a free-text customField
// (the txt01 case) additionally needs a valueHint, because the model cannot
// see real values the way it can an enum's `values` list — description says
// what the field IS, valueHint says what it typically CONTAINS, and only the
// second one lets the model use `contains`/`regex` sensibly.
func TestCustomFieldSearchableStringRequiresValueHint(t *testing.T) {
	cases := map[string]string{
		"no valueHint at all": `{"name":"txt01","type":"string","customField":true,"description":"agent notes"}`,
		"empty valueHint":     `{"name":"txt01","type":"string","customField":true,"description":"agent notes","valueHint":""}`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			js := `{"entity":"Order","fields":[` + field + `]}`
			_, err := ParseConfig([]byte(js))
			if err == nil {
				t.Fatalf("expected rejection (searchable string customField missing valueHint), got none")
			}
			if !strings.Contains(err.Error(), "valueHint") {
				t.Errorf("error should name the missing valueHint, got: %v", err)
			}
		})
	}
}

// TestCustomFieldNonSearchableStringDoesNotNeedValueHint: a string field with
// searchable explicitly turned off has no free-text operator (no contains/
// regex) — there is nothing for a valueHint to give context to, so only
// description is required.
func TestCustomFieldNonSearchableStringDoesNotNeedValueHint(t *testing.T) {
	mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "code01", "type": "string", "searchable": false, "customField": true,
	     "description": "internal status code, not free text"}
	  ]
	}`)
}

// TestCustomFieldEnumStillNeedsValues proves the pre-existing "enum must list
// values" rule and the new customField rule interact correctly rather than
// one silently disabling the other: an enum customField with a description
// but no values is still rejected.
func TestCustomFieldEnumStillNeedsValues(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "entity": "Order",
	  "fields": [
	    {"name": "sel01", "type": "enum", "customField": true, "description": "visa type"}
	  ]
	}`))
	if err == nil {
		t.Fatalf("expected rejection: enum field with no values, customField or not")
	}
}

// TestCustomFieldEnumWithDescriptionAndValuesLoads: the positive counterpart —
// once both requirements are satisfied (values for the enum domain,
// description for the customField rule), the config loads.
func TestCustomFieldEnumWithDescriptionAndValuesLoads(t *testing.T) {
	mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "sel02", "type": "enum", "customField": true,
	     "description": "visa type for this application",
	     "values": ["TOURIST", "WORK", "STUDENT"]}
	  ]
	}`)
}

// TestCustomFieldNonStringTypesOnlyNeedDescription pins the explicit business
// decision from this session (no min/max requirement for numeric custom
// fields): a number, boolean, or date customField is fully satisfied by a
// description alone.
func TestCustomFieldNonStringTypesOnlyNeedDescription(t *testing.T) {
	mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "num01", "type": "number", "customField": true, "description": "loyalty points balance"},
	    {"name": "flag01", "type": "boolean", "customField": true, "description": "internal QA flag"},
	    {"name": "date03", "type": "date", "customField": true, "description": "last review date"}
	  ]
	}`)
}

// TestCustomFieldArrayOnlyNeedsDescription: an array field (a tags-like custom
// multi-select) is not "free text" in the sense valueHint exists for — only a
// plain FieldString carries the valueHint requirement.
func TestCustomFieldArrayOnlyNeedsDescription(t *testing.T) {
	mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "arr01", "type": "array", "itemType": "string", "customField": true,
	     "description": "per-tenant custom multi-select tags"}
	  ]
	}`)
}

// TestValueHintRejectedOnNonFreeTextField: valueHint only means something on a
// searchable string field. Setting it elsewhere — regardless of customField —
// is a config mistake caught at load, the same strictness ValueCase and
// KeywordMapping already apply to fields their meaning cannot reach.
func TestValueHintRejectedOnNonFreeTextField(t *testing.T) {
	cases := map[string]string{
		"number field":          `{"name":"amount","type":"number","valueHint":"typically 10-500"}`,
		"non-searchable string": `{"name":"code","type":"string","searchable":false,"valueHint":"a note"}`,
		"enum field":            `{"name":"status","type":"enum","values":["A","B"],"valueHint":"a note"}`,
		"boolean field":         `{"name":"flag","type":"boolean","valueHint":"a note"}`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			js := `{"entity":"Order","fields":[` + field + `]}`
			_, err := ParseConfig([]byte(js))
			if err == nil {
				t.Fatalf("expected rejection (valueHint on a field it cannot apply to), got none")
			}
			if !strings.Contains(err.Error(), "valueHint") {
				t.Errorf("error should name valueHint, got: %v", err)
			}
		})
	}
}

// TestMetadataLengthCeilings: an author pasting a paragraph into description
// or valueHint bloats every future prompt this config ever sends. Rejected at
// load rather than silently accepted and paid for on every translate call.
func TestMetadataLengthCeilings(t *testing.T) {
	tooLongDisplayName := strings.Repeat("a", config.MaxDisplayNameLength+1)
	tooLongDescription := strings.Repeat("a", config.MaxDescriptionLength+1)
	tooLongHint := strings.Repeat("a", config.MaxValueHintLength+1)

	cases := map[string]string{
		"displayName over limit": `{"name":"a","type":"string","displayName":"` + tooLongDisplayName + `"}`,
		"description over limit": `{"name":"a","type":"string","description":"` + tooLongDescription + `"}`,
		"valueHint over limit":   `{"name":"a","type":"string","valueHint":"` + tooLongHint + `"}`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			js := `{"entity":"Order","fields":[` + field + `]}`
			if _, err := ParseConfig([]byte(js)); err == nil {
				t.Fatalf("expected rejection for oversized metadata")
			}
		})
	}
}

// TestMetadataAtExactLengthCeilingIsAccepted is the boundary case for the
// above: the limit itself must not be treated as already over it.
func TestMetadataAtExactLengthCeilingIsAccepted(t *testing.T) {
	exactDescription := strings.Repeat("a", config.MaxDescriptionLength)
	mustParse(t, `{
	  "entity": "Order",
	  "fields": [
	    {"name": "a", "type": "string", "description": "`+exactDescription+`"}
	  ]
	}`)
}

// TestCustomFieldHiddenFieldStillRequiresDescription documents a deliberate
// simplicity choice made this session: a field marked both customField:true
// and queryable:false (so it will never actually reach the model) still must
// carry a description. One unconditional rule is easier for a config author
// to reason about than "it depends on whether the field happens to be
// hidden", and the cost of one stray sentence on a hidden field is near zero.
func TestCustomFieldHiddenFieldStillRequiresDescription(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "entity": "Order",
	  "fields": [
	    {"name": "secretCustom", "type": "string", "searchable": false, "customField": true, "queryable": false}
	  ]
	}`))
	if err == nil {
		t.Fatalf("expected rejection: customField still requires description even when hidden from the model")
	}
}

// TestCustomFieldMetadataNeverReachesGeneratedQuery is the negative guarantee
// that matters most from a correctness standpoint: displayName/description/
// valueHint are prompt-only. Two configs identical except for Tier-1 metadata
// must compile the SAME AST to byte-identical SQL, MySQL, and Mongo output —
// proving the generators are completely untouched by this feature and cannot
// leak this metadata (or a customField flag) into a real query.
func TestCustomFieldMetadataNeverReachesGeneratedQuery(t *testing.T) {
	plain := mustParse(t, `{
	  "entity": "Order",
	  "backends": {"sql": {"table": "orders"}, "mongo": {"collection": "orders"}},
	  "fields": [
	    {"name": "txt01", "type": "string", "mapping": {"sql": "txt01", "mongo": "txt01"}},
	    {"name": "amount", "type": "number", "mapping": {"sql": "amount", "mongo": "amount"}}
	  ]
	}`)
	annotated := mustParse(t, `{
	  "entity": "Order",
	  "backends": {"sql": {"table": "orders"}, "mongo": {"collection": "orders"}},
	  "fields": [
	    {"name": "txt01", "type": "string", "customField": true,
	     "displayName": "Passport Country", "description": "ISO country of issuance",
	     "valueHint": "Free-text notes entered by agents about travel documents.",
	     "mapping": {"sql": "txt01", "mongo": "txt01"}},
	    {"name": "amount", "type": "number", "mapping": {"sql": "amount", "mongo": "amount"}}
	  ]
	}`)

	q := NewQuery("Order")
	q.Filter = and(
		comp("txt01", OpContains, vStr("France")),
		comp("amount", OpGt, vNum(100)),
	)

	for _, tc := range []struct {
		name string
		gen  Generator
	}{
		{"postgres", SQLGenerator{}},
		{"mysql", MySQLGenerator{}},
		{"mongo", MongoGenerator{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plainResult, err := tc.gen.Generate(q, plain, GenOptions{Now: fixedNow})
			if err != nil {
				t.Fatalf("generate against plain config: %v", err)
			}
			annotatedResult, err := tc.gen.Generate(q, annotated, GenOptions{Now: fixedNow})
			if err != nil {
				t.Fatalf("generate against annotated config: %v", err)
			}
			if plainResult.SQL != annotatedResult.SQL {
				t.Errorf("SQL differs by metadata alone:\n plain: %s\nannot.: %s", plainResult.SQL, annotatedResult.SQL)
			}
			if len(plainResult.Args) != len(annotatedResult.Args) {
				t.Errorf("arg count differs: plain=%v annotated=%v", plainResult.Args, annotatedResult.Args)
			}
			// Mongo has no SQL text; compare the doc form directly.
			if tc.name == "mongo" {
				pm := plainResult.Doc.(*MongoQuery)
				am := annotatedResult.Doc.(*MongoQuery)
				if pm.Collection != am.Collection {
					t.Errorf("mongo collection differs: %q vs %q", pm.Collection, am.Collection)
				}
				if len(pm.Filter) != len(am.Filter) {
					t.Errorf("mongo filter shape differs: %#v vs %#v", pm.Filter, am.Filter)
				}
			}
		})
	}
}
