package queryforge

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the single source of truth. It is simultaneously the prompt
// context (which fields/operators/enums the model may use), the validation
// rulebook, the field-to-backend mapping, and the model selector. JSON is
// supported out of the box with the standard library only; YAML is a planned
// optional add-on.
type Config struct {
	Entity  string      `json:"entity"`
	Version int         `json:"version,omitempty"`
	Model   ModelConfig `json:"model"`

	// Models is an optional ordered fallback chain. When set, the engine tries
	// Model first (if non-empty) then each entry here in order, using the first
	// that answers — so an outage, rate-limit, or quota/billing block on one
	// provider transparently falls through to the next. Empty = just Model.
	Models []ModelConfig `json:"models,omitempty"`

	Backends map[string]BackendConfig `json:"backends,omitempty"`
	Fields   []Field                  `json:"fields"`
	Defaults Defaults                 `json:"defaults,omitempty"`
	Policy   Policy                   `json:"policy,omitempty"`

	// Built at load time; not serialized.
	fieldByName  map[string]*Field
	synonymIndex map[string]*Field
}

// ModelConfig selects the AI planner target. Swapping provider or model is a
// config change, never a code change: any OpenAI-compatible /chat/completions
// endpoint works. APIKeyEnv is the NAME of the environment variable holding
// the key, never the key itself.
type ModelConfig struct {
	Provider    string  `json:"provider,omitempty"`
	BaseURL     string  `json:"baseURL,omitempty"`
	Model       string  `json:"model,omitempty"`
	APIKeyEnv   string  `json:"apiKeyEnv,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"maxTokens,omitempty"`

	// JSONMode opts into the OpenAI response_format=json_object flag. It is off
	// by default because it is not uniformly implemented: Gemini's
	// OpenAI-compatibility endpoint returns brace-unbalanced JSON with it set
	// (measured: 2/5 replies parseable with it, 5/5 without), while the prompt
	// already demands a bare JSON object and the parser tolerates code fences.
	// Turn it on for endpoints where it genuinely helps.
	JSONMode *bool `json:"jsonMode,omitempty"`
}

// EffectiveJSONMode reports whether to send response_format=json_object.
// Defaults to false — see JSONMode.
func (m ModelConfig) EffectiveJSONMode() bool { return m.JSONMode != nil && *m.JSONMode }

// isEmpty reports whether the block names no endpoint at all — used to skip a
// blank primary Model when only the Models fallback list is populated.
func (m ModelConfig) isEmpty() bool {
	return m.Provider == "" && m.BaseURL == "" && m.Model == ""
}

// Label is a short human name for the model (for telemetry and error messages),
// derived from the most specific field present.
func (m ModelConfig) Label() string {
	switch {
	case m.Provider != "" && m.Model != "":
		return m.Provider + "/" + m.Model
	case m.Model != "":
		return m.Model
	case m.Provider != "":
		return m.Provider
	case m.BaseURL != "":
		return m.BaseURL
	default:
		return "model"
	}
}

// BackendConfig maps the logical entity to a physical source per backend. Each
// backend uses its idiomatic key; Source returns whichever is set.
type BackendConfig struct {
	Table      string `json:"table,omitempty"`
	Collection string `json:"collection,omitempty"`
	Index      string `json:"index,omitempty"`
	Name       string `json:"name,omitempty"` // generic fallback for custom backends
}

// Source returns the configured physical source name for the backend.
func (b BackendConfig) Source() string {
	for _, s := range []string{b.Table, b.Collection, b.Index, b.Name} {
		if s != "" {
			return s
		}
	}
	return ""
}

// FieldType is the logical type of a field. It bounds which operators and
// value kinds are legal for that field.
type FieldType string

const (
	FieldString  FieldType = "string"
	FieldNumber  FieldType = "number"
	FieldBoolean FieldType = "boolean"
	FieldEnum    FieldType = "enum"
	FieldDate    FieldType = "date"
	FieldArray   FieldType = "array"
)

// Field is one registered, queryable attribute of the entity. Beyond its type
// and operator vocabulary it carries capability flags (what operations it
// supports) and hints (indexed/priority) that shape the generated query and
// the model prompt. Capability flags are pointers so an unset flag can take a
// type-aware default distinct from an explicit false; see the Effective*
// accessors below.
type Field struct {
	Name      string     `json:"name"`                // logical field name used in the AST
	Type      FieldType  `json:"type"`                // logical type; bounds legal operators and value kinds
	Values    []string   `json:"values,omitempty"`    // enum domain (required when type == enum)
	ItemType  FieldType  `json:"itemType,omitempty"`  // element type for array fields
	Operators []Operator `json:"operators,omitempty"` // explicit comparison-operator whitelist; empty = type defaults
	Synonyms  []string   `json:"synonyms,omitempty"`  // alternate phrasings that resolve to this field

	// mapping decouples the logical name from the physical column/field per
	// backend, e.g. {"sql":"customer_name","mongo":"customerName"}.
	Mapping map[string]string `json:"mapping,omitempty"`

	// --- Capability flags: what operations this field may take part in. ---
	Queryable  *bool `json:"queryable,omitempty"`  // include/exclude from the NLP surface; false = hidden + rejected if referenced (default true)
	Filterable *bool `json:"filterable,omitempty"` // may appear in structured filter predicates (default true)
	Searchable *bool `json:"searchable,omitempty"` // may use text-search operators (contains/startsWith/endsWith/regex) (default: true for strings, else false)
	Sortable   *bool `json:"sortable,omitempty"`   // may appear in sort[] (default: false for arrays, else true)
	Returnable *bool `json:"returnable,omitempty"` // may appear in the result projection/select (default true)

	// --- Hints: do not gate anything, but steer ordering and the prompt. ---
	Indexed  bool `json:"indexed,omitempty"`  // field is backed by a DB index; used for predicate ordering + soft warnings
	Priority int  `json:"priority,omitempty"` // relative importance; higher sorts earlier in predicates and prompt

	Permissions *FieldPermissions `json:"permissions,omitempty"` // field-level RBAC (read roles)
	Validators  *FieldValidators  `json:"validators,omitempty"`  // deterministic value constraints (numeric bounds)
}

// FieldPermissions carries field-level RBAC. Read lists the roles that may see
// and query the field; "*" means everyone.
type FieldPermissions struct {
	Read []string `json:"read,omitempty"`
}

// FieldValidators are deterministic value constraints applied during
// validation (currently numeric bounds).
type FieldValidators struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// Defaults shape unspecified result windows.
type Defaults struct {
	Limit    int `json:"limit,omitempty"`
	MaxLimit int `json:"maxLimit,omitempty"`
}

// Policy encodes deterministic guardrails.
type Policy struct {
	RequireTenantPredicate bool     `json:"requireTenantPredicate,omitempty"`
	MaxNestingDepth        int      `json:"maxNestingDepth,omitempty"`
	DenyRegexOn            []string `json:"denyRegexOn,omitempty"`
}

// LoadConfig reads and parses a JSON config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses JSON config bytes, builds lookup indexes, and runs
// structural self-validation so a broken config fails at load, not mid-query.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// finalize builds indexes and validates the config's own integrity.
func (c *Config) finalize() error {
	if strings.TrimSpace(c.Entity) == "" {
		return fmt.Errorf("config: entity is required")
	}
	if len(c.Fields) == 0 {
		return fmt.Errorf("config: at least one field is required")
	}
	c.fieldByName = make(map[string]*Field, len(c.Fields))
	c.synonymIndex = make(map[string]*Field)
	for i := range c.Fields {
		f := &c.Fields[i]
		if f.Name == "" {
			return fmt.Errorf("config: field #%d has no name", i)
		}
		if !validFieldType(f.Type) {
			return fmt.Errorf("config: field %q has invalid type %q", f.Name, f.Type)
		}
		if f.Type == FieldEnum && len(f.Values) == 0 {
			return fmt.Errorf("config: enum field %q must list values", f.Name)
		}
		for _, op := range f.Operators {
			if !isKnownOperator(op) {
				return fmt.Errorf("config: field %q lists unknown operator %q", f.Name, op)
			}
		}
		if _, dup := c.fieldByName[f.Name]; dup {
			return fmt.Errorf("config: duplicate field %q", f.Name)
		}
		c.fieldByName[f.Name] = f
		c.synonymIndex[strings.ToLower(f.Name)] = f
		for _, syn := range f.Synonyms {
			c.synonymIndex[strings.ToLower(syn)] = f
		}
	}
	return nil
}

// FieldByName returns the registered field with the given logical name.
func (c *Config) FieldByName(name string) (*Field, bool) {
	f, ok := c.fieldByName[name]
	return f, ok
}

// ResolveSynonym maps a lowercased name or synonym to its field (used by the
// planner's field-resolution hints and by tooling).
func (c *Config) ResolveSynonym(term string) (*Field, bool) {
	f, ok := c.synonymIndex[strings.ToLower(strings.TrimSpace(term))]
	return f, ok
}

// FieldNames returns all registered logical field names in config order.
func (c *Config) FieldNames() []string {
	names := make([]string, len(c.Fields))
	for i := range c.Fields {
		names[i] = c.Fields[i].Name
	}
	return names
}

// PhysicalName returns the backend-specific column/field name for a logical
// field, falling back to the logical name when no mapping is declared.
func (c *Config) PhysicalName(fieldName, backend string) string {
	f, ok := c.fieldByName[fieldName]
	if !ok {
		return fieldName
	}
	if phys, ok := f.Mapping[backend]; ok && phys != "" {
		return phys
	}
	return fieldName
}

// EffectiveOperators returns the operators a field permits. When the config
// lists none explicitly, a sensible default set is derived from the type so
// configs stay terse.
func (f Field) EffectiveOperators() []Operator {
	if len(f.Operators) > 0 {
		return f.Operators
	}
	return defaultOperatorsFor(f.Type)
}

// AllowsOperator reports whether the field permits the operator.
func (f Field) AllowsOperator(op Operator) bool {
	for _, o := range f.EffectiveOperators() {
		if o == op {
			return true
		}
	}
	return false
}

// ReadableBy reports whether a caller holding the given roles may read the
// field. A field with no permissions block is readable by everyone.
func (f Field) ReadableBy(roles []string) bool {
	if f.Permissions == nil || len(f.Permissions.Read) == 0 {
		return true
	}
	for _, allowed := range f.Permissions.Read {
		if allowed == "*" {
			return true
		}
		for _, r := range roles {
			if r == allowed {
				return true
			}
		}
	}
	return false
}

// EffectiveQueryable reports whether the field is exposed to the NLP layer and
// may be referenced at all. Defaults to true when unset.
func (f Field) EffectiveQueryable() bool { return boolOr(f.Queryable, true) }

// EffectiveFilterable reports whether the field may appear in filter
// predicates. Defaults to true when unset.
func (f Field) EffectiveFilterable() bool { return boolOr(f.Filterable, true) }

// EffectiveSearchable reports whether the field may use text-search operators.
// Defaults to true for string fields and false for every other type.
func (f Field) EffectiveSearchable() bool { return boolOr(f.Searchable, f.Type == FieldString) }

// EffectiveSortable reports whether the field may appear in sort[]. Defaults to
// false for arrays (ordering by an array is ill-defined) and true otherwise.
func (f Field) EffectiveSortable() bool { return boolOr(f.Sortable, f.Type != FieldArray) }

// EffectiveReturnable reports whether the field may appear in a projection.
// Defaults to true when unset.
func (f Field) EffectiveReturnable() bool { return boolOr(f.Returnable, true) }

// boolOr returns the pointed-to bool, or def when the pointer is nil (unset).
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// isTextSearchOperator reports whether op is a string-matching operator. On a
// string/enum field these require EffectiveSearchable; on an array field the
// same tokens (e.g. contains) mean membership and are governed by filterable
// instead, so callers must scope this check by field type.
func isTextSearchOperator(op Operator) bool {
	switch op {
	case OpContains, OpStartsWith, OpEndsWith, OpRegex:
		return true
	}
	return false
}

func defaultOperatorsFor(t FieldType) []Operator {
	switch t {
	case FieldString:
		return []Operator{OpEquals, OpNotEquals, OpContains, OpStartsWith, OpEndsWith, OpIn, OpNotIn, OpIsNull, OpIsNotNull}
	case FieldNumber:
		return []Operator{OpEquals, OpNotEquals, OpGt, OpLt, OpGte, OpLte, OpBetween, OpIn, OpNotIn, OpIsNull, OpIsNotNull}
	case FieldBoolean:
		return []Operator{OpEquals, OpNotEquals, OpIsNull, OpIsNotNull}
	case FieldEnum:
		return []Operator{OpEquals, OpNotEquals, OpIn, OpNotIn, OpIsNull, OpIsNotNull}
	case FieldDate:
		return []Operator{OpBefore, OpAfter, OpBetween, OpEquals, OpIsNull, OpIsNotNull}
	case FieldArray:
		return []Operator{OpContains, OpContainsAny, OpContainsAll, OpIsNull, OpIsNotNull}
	default:
		return nil
	}
}

func validFieldType(t FieldType) bool {
	switch t {
	case FieldString, FieldNumber, FieldBoolean, FieldEnum, FieldDate, FieldArray:
		return true
	}
	return false
}

func isKnownOperator(op Operator) bool {
	for _, o := range AllOperators {
		if o == op {
			return true
		}
	}
	return false
}
