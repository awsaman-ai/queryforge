package queryforge

import (
	"strings"
	"testing"
)

// fullConfigJSON is a representative config that uses every documented key.
// Parsing it clean is the direct check for BUG-001 (DisallowUnknownFields
// rejecting a legitimate documented key).
const fullConfigJSON = `{
  "entity": "Order",
  "version": 3,
  "model": {
    "provider": "gemini",
    "baseURL": "https://generativelanguage.googleapis.com/v1beta/openai",
    "model": "gemini-2.0-flash",
    "apiKeyEnv": "QF_API_KEY",
    "temperature": 0,
    "maxTokens": 1024
  },
  "backends": {
    "sql":   { "table": "orders" },
    "mongo": { "collection": "orders" },
    "es":    { "index": "orders-v2" }
  },
  "fields": [
    {
      "name": "status", "type": "enum",
      "values": ["PLACED","DELIVERED","CANCELLED","REFUNDED"],
      "operators": ["equals","notEquals","in","notIn"],
      "synonyms": ["state","order status"],
      "indexed": true, "priority": 10,
      "mapping": { "sql": "status", "mongo": "status", "es": "status" },
      "permissions": { "read": ["*"] }
    },
    {
      "name": "createdAt", "type": "date",
      "operators": ["before","after","between"],
      "synonyms": ["created","order date"],
      "indexed": true, "priority": 8,
      "mapping": { "sql": "created_at", "mongo": "createdAt" }
    },
    {
      "name": "amount", "type": "number",
      "operators": ["gt","lt","gte","lte","between"],
      "synonyms": ["total","price"],
      "validators": { "min": 0 }
    },
    {
      "name": "tags", "type": "array", "itemType": "string",
      "operators": ["contains","containsAny","containsAll"]
    },
    {
      "name": "customerName", "type": "string",
      "operators": ["contains","startsWith","equals"],
      "searchable": true,
      "permissions": { "read": ["support","admin"] }
    },
    {
      "name": "internalNote", "type": "string",
      "queryable": false
    }
  ],
  "defaults": { "limit": 50, "maxLimit": 500 },
  "policy": {
    "requireTenantPredicate": true,
    "maxNestingDepth": 5,
    "denyRegexOn": ["customerName"]
  }
}`

func mustParse(t *testing.T, js string) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(js))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return c
}

// TestFullConfigParses is the happy path and clears BUG-001.
func TestFullConfigParses(t *testing.T) {
	c := mustParse(t, fullConfigJSON)
	if c.Entity != "Order" {
		t.Errorf("entity = %q", c.Entity)
	}
	if len(c.Fields) != 6 {
		t.Fatalf("expected 6 fields, got %d", len(c.Fields))
	}
	if c.Model.Model != "gemini-2.0-flash" {
		t.Errorf("model block not parsed: %+v", c.Model)
	}
	if c.Defaults.MaxLimit != 500 {
		t.Errorf("defaults not parsed: %+v", c.Defaults)
	}
	if !c.Policy.RequireTenantPredicate {
		t.Errorf("policy not parsed: %+v", c.Policy)
	}
}

// TestIndexingAndLookups covers the built lookup maps and physical mapping.
func TestIndexingAndLookups(t *testing.T) {
	c := mustParse(t, fullConfigJSON)

	if _, ok := c.FieldByName("status"); !ok {
		t.Errorf("status should be found by name")
	}
	// synonym resolution is case-insensitive
	if f, ok := c.ResolveSynonym("STATE"); !ok || f.Name != "status" {
		t.Errorf("synonym 'STATE' did not resolve to status")
	}
	// mapping fallback: createdAt has no es mapping -> logical name
	if got := c.PhysicalName("createdAt", "es"); got != "createdAt" {
		t.Errorf("es fallback mapping = %q, want createdAt", got)
	}
	if got := c.PhysicalName("createdAt", "sql"); got != "created_at" {
		t.Errorf("sql mapping = %q, want created_at", got)
	}
}

// TestCapabilityDefaults verifies the type-aware Effective* defaults and that
// an explicit flag overrides them.
func TestCapabilityDefaults(t *testing.T) {
	c := mustParse(t, fullConfigJSON)
	get := func(name string) *Field { f, _ := c.FieldByName(name); return f }

	amount := get("amount") // number
	if !amount.EffectiveFilterable() || !amount.EffectiveSortable() || !amount.EffectiveReturnable() {
		t.Errorf("number defaults wrong: %+v", amount)
	}
	if amount.EffectiveSearchable() {
		t.Errorf("number should not be searchable by default")
	}

	name := get("customerName") // string
	if !name.EffectiveSearchable() {
		t.Errorf("string should be searchable by default")
	}

	tags := get("tags") // array
	if tags.EffectiveSortable() {
		t.Errorf("array should not be sortable by default")
	}

	internal := get("internalNote") // queryable:false explicitly
	if internal.EffectiveQueryable() {
		t.Errorf("internalNote queryable override to false ignored")
	}
}

// TestEffectiveOperatorsDefaulting checks the derived operator set for a field
// that lists none.
func TestEffectiveOperatorsDefaulting(t *testing.T) {
	c := mustParse(t, `{
      "entity":"X","model":{},
      "fields":[{"name":"n","type":"number"}]
    }`)
	f, _ := c.FieldByName("n")
	if !f.AllowsOperator(OpBetween) || f.AllowsOperator(OpContains) {
		t.Errorf("number default operators wrong: %v", f.EffectiveOperators())
	}
}

// --- Adversarial / worst-case parsing: each MUST be rejected. ---

func TestConfigRejections(t *testing.T) {
	cases := map[string]string{
		"empty entity":       `{"entity":"","model":{},"fields":[{"name":"a","type":"string"}]}`,
		"no fields":          `{"entity":"X","model":{},"fields":[]}`,
		"duplicate field":    `{"entity":"X","model":{},"fields":[{"name":"a","type":"string"},{"name":"a","type":"number"}]}`,
		"enum no values":     `{"entity":"X","model":{},"fields":[{"name":"a","type":"enum"}]}`,
		"invalid field type": `{"entity":"X","model":{},"fields":[{"name":"a","type":"blob"}]}`,
		"unknown operator":   `{"entity":"X","model":{},"fields":[{"name":"a","type":"string","operators":["frobnicate"]}]}`,
		"unnamed field":      `{"entity":"X","model":{},"fields":[{"type":"string"}]}`,
		"unknown top key":    `{"entity":"X","model":{},"fields":[{"name":"a","type":"string"}],"bogus":true}`,
		"unknown field key":  `{"entity":"X","model":{},"fields":[{"name":"a","type":"string","sortabler":true}]}`,
		"not json":           `nope`,
	}
	for name, js := range cases {
		if _, err := ParseConfig([]byte(js)); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
}

// TestLoadConfigMissingFile is the worst-case for the file loader.
func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig("/no/such/queryforge-config.json"); err == nil {
		t.Errorf("expected error loading a missing file")
	}
}

// TestReadableBy exercises the field-level RBAC helper.
func TestReadableBy(t *testing.T) {
	c := mustParse(t, fullConfigJSON)
	status, _ := c.FieldByName("status")     // read: ["*"]
	cust, _ := c.FieldByName("customerName") // read: ["support","admin"]

	if !status.ReadableBy(nil) {
		t.Errorf("wildcard field should be readable by anyone")
	}
	if cust.ReadableBy([]string{"guest"}) {
		t.Errorf("guest must not read customerName")
	}
	if !cust.ReadableBy([]string{"admin"}) {
		t.Errorf("admin must read customerName")
	}
}

// TestApiKeyEnvNeverHoldsSecret is a guard on the design invariant that the
// config carries the env var NAME, not the key itself.
func TestApiKeyEnvNeverHoldsSecret(t *testing.T) {
	c := mustParse(t, fullConfigJSON)
	if strings.ContainsAny(c.Model.APIKeyEnv, "=/:") || len(c.Model.APIKeyEnv) > 64 {
		t.Errorf("apiKeyEnv %q looks like a secret, not an env var name", c.Model.APIKeyEnv)
	}
}
