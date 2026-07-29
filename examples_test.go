package queryforge

import (
	"path/filepath"
	"testing"
)

// TestExampleConfigsParse loads every shipped example config and confirms it
// passes structural self-validation — the examples must always be correct since
// users copy them.
func TestExampleConfigsParse(t *testing.T) {
	files, err := filepath.Glob("examples/*.config.json")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) < 3 {
		t.Fatalf("expected at least 3 example configs, found %d", len(files))
	}

	for _, f := range files {
		c, err := LoadConfig(f)
		if err != nil {
			t.Errorf("%s: failed to load: %v", f, err)
			continue
		}
		if c.Entity == "" || len(c.Fields) == 0 {
			t.Errorf("%s: empty entity or fields", f)
		}
		// The model block must reference an env var NAME, never a literal key.
		if c.Model.APIKeyEnv == "" {
			t.Errorf("%s: model.apiKeyEnv is empty", f)
		}
		// Every field must be individually valid (type known, enum has values).
		for _, fld := range c.Fields {
			if !validFieldType(fld.Type) {
				t.Errorf("%s: field %q has bad type %q", f, fld.Name, fld.Type)
			}
		}
	}
}

// TestExampleConfigsGenerate builds an engine per example and compiles a simple
// hand-built AST to each backend the config declares — proving the examples are
// usable end-to-end on the deterministic path (no model call).
func TestExampleConfigsGenerate(t *testing.T) {
	cases := map[string]struct {
		field    string
		backends []string
	}{
		"examples/orders.config.json":         {"status", []string{"sql", "mongo"}},
		"examples/sql_employees.config.json":  {"department", []string{"sql"}},
		"examples/nosql_products.config.json": {"brand", []string{"mongo"}},
	}
	for file, tc := range cases {
		c, err := LoadConfig(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		e := NewWithProvider(c, &StubProvider{})

		// A minimal equals predicate on an enum field of that config.
		fld, ok := c.FieldByName(tc.field)
		if !ok || fld.Type != FieldEnum || len(fld.Values) == 0 {
			t.Fatalf("%s: expected enum field %q", file, tc.field)
		}
		ast := NewQuery(c.Entity)
		ast.Filter = comp(tc.field, OpEquals, vEnum(fld.Values[0]))

		for _, backend := range tc.backends {
			if _, err := e.GenerateFrom(ast, backend); err != nil {
				t.Errorf("%s/%s: GenerateFrom failed: %v", file, backend, err)
			}
		}
	}
}
