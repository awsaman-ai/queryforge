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

// TestAPIKeyEnvRejectsPastedSecret covers BUG-009. apiKeyEnv holds the NAME of
// an environment variable; pasting the key itself produced no Authorization
// header and a provider-side "Missing or invalid Authorization header" 400 —
// an error that points nowhere near the actual cause. Loading must fail instead.
func TestAPIKeyEnvRejectsPastedSecret(t *testing.T) {
	cases := []struct {
		name, apiKeyEnv string
	}{
		{"gemini key", "AIzaSyFAKE0000000000000000000000000000"},
		{"openai key", "sk-proj-abc123def456"},
		{"anthropic key", "sk-ant-api03-abc123"},
		{"groq key", "gsk_abc123def456"},
		{"github token", "ghp_abc123def456"},
		{"slack token", "xoxb-123-456-abc"},
		{"contains a dash", "QF-API-KEY"},
		{"contains a dot", "qf.api.key"},
		{"contains a space", "QF API KEY"},
		{"starts with a digit", "1KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			js := `{"entity":"Order","model":{"baseURL":"https://x","model":"m","apiKeyEnv":"` + tc.apiKeyEnv + `"},
			        "fields":[{"name":"status","type":"string"}]}`
			_, err := ParseConfig([]byte(js))
			if err == nil {
				t.Fatalf("expected %q to be rejected", tc.apiKeyEnv)
			}
			if !strings.Contains(err.Error(), "apiKeyEnv") {
				t.Errorf("error should name the offending field, got: %v", err)
			}
			// The error must never reprint the secret — it would land in logs.
			if strings.Contains(err.Error(), tc.apiKeyEnv) {
				t.Errorf("error echoed the offending value back: %v", err)
			}
		})
	}
}

// TestAPIKeyEnvAcceptsValidNames guards against over-rejecting: legitimate
// variable names, and an empty value (keyless local servers), must still load.
func TestAPIKeyEnvAcceptsValidNames(t *testing.T) {
	for _, name := range []string{"QF_API_KEY", "ANTHROPIC_API_KEY", "_KEY", "key2", "A1"} {
		js := `{"entity":"Order","model":{"baseURL":"https://x","model":"m","apiKeyEnv":"` + name + `"},
		        "fields":[{"name":"status","type":"string"}]}`
		if _, err := ParseConfig([]byte(js)); err != nil {
			t.Errorf("%q should be accepted, got %v", name, err)
		}
	}
	// Omitted entirely: legal, for keyless endpoints such as a local Ollama.
	js := `{"entity":"Order","model":{"baseURL":"http://localhost:11434/v1","model":"qwen2.5"},
	        "fields":[{"name":"status","type":"string"}]}`
	if _, err := ParseConfig([]byte(js)); err != nil {
		t.Errorf("absent apiKeyEnv should be accepted, got %v", err)
	}
}

// TestAPIKeyEnvCheckedInFallbackChain checks every entry in the models[] chain
// is validated, not just the primary block.
func TestAPIKeyEnvCheckedInFallbackChain(t *testing.T) {
	js := `{"entity":"Order",
	        "model":{"baseURL":"https://x","model":"m","apiKeyEnv":"QF_API_KEY"},
	        "models":[{"baseURL":"https://y","model":"n","apiKeyEnv":"AIzaSyEXAMPLEEXAMPLE"}],
	        "fields":[{"name":"status","type":"string"}]}`
	err := func() error { _, e := ParseConfig([]byte(js)); return e }()
	if err == nil {
		t.Fatal("expected a pasted key in models[0] to be rejected")
	}
	if !strings.Contains(err.Error(), "models[0]") {
		t.Errorf("error should identify which chain entry is wrong, got: %v", err)
	}
}
