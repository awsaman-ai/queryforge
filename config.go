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

// secretPrefixes are the recognisable openings of common API keys. Matching one
// means the caller pasted a credential where a variable name belongs.
var secretPrefixes = []string{
	"AIza",    // Google / Gemini
	"sk-",     // OpenAI and compatible
	"sk-ant-", // Anthropic
	"gsk_",    // Groq
	"ghp_",    // GitHub personal access token
	"xox",     // Slack
}

// validateAPIKeyEnv rejects an apiKeyEnv that holds a secret rather than the
// name of an environment variable.
//
// This exists because the failure it prevents is almost impossible to diagnose
// from the outside: the key never reaches the provider, so the endpoint replies
// "Missing or invalid Authorization header" — an error about a header the caller
// never saw, describing a key they are certain they configured. Worse, the
// pasted secret then travels wherever the config travels, including into version
// control and startup logs.
//
// The rule: environment variable names are letters, digits and underscores, and
// do not begin with a digit. Real keys violate this immediately (they contain
// "-", or start with a lowercase provider prefix), so the check is precise
// without being clever.
//
// Errors never quote the offending value — echoing it would reprint the secret
// in logs, which is part of what this guards against.
func (m ModelConfig) validateAPIKeyEnv(where string) error {
	name := strings.TrimSpace(m.APIKeyEnv)
	if name == "" {
		return nil // absent is legal: keyless local servers (Ollama) need no key
	}

	// A recognised key prefix is unambiguous, so name it directly.
	for _, p := range secretPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("config: %s.apiKeyEnv looks like an API key, not an environment variable name. "+
				"This field takes the NAME of the variable holding your key (e.g. \"QF_API_KEY\"), and the key itself "+
				"goes in the environment: export QF_API_KEY=<your-key>. Keys must never be written into a config file", where)
		}
	}

	// Otherwise fall back to the shape of a legal variable name.
	if !validEnvVarName(name) {
		return fmt.Errorf("config: %s.apiKeyEnv is not a valid environment variable name "+
			"(expected letters, digits and underscores, not starting with a digit — e.g. \"QF_API_KEY\"). "+
			"This field takes the variable's NAME; the key itself belongs in the environment", where)
	}
	return nil
}

// validEnvVarName reports whether s has the shape of a POSIX environment
// variable name: [A-Za-z_][A-Za-z0-9_]*.
func validEnvVarName(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch == '_':
			// always legal
		case ch >= '0' && ch <= '9':
			if i == 0 {
				return false // a name may not start with a digit
			}
		default:
			return false // "-", ".", spaces, etc.
		}
	}
	return true
}

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
	// backend, e.g. {"sql":"customer_name","mongo":"customerName"}. For Mongo the
	// value may be a dot path into an embedded document ("address.city"), which
	// is written straight into the filter key.
	Mapping map[string]string `json:"mapping,omitempty"`

	// ElemMatch names the Mongo array-of-sub-documents this field lives inside,
	// as a dot path from the document root (e.g. "items" for a field mapped to
	// "items.sku"). It is Mongo-only; every other backend ignores it.
	//
	// It exists because dot notation alone is silently wrong on an array of
	// sub-documents. {"items.sku":"ABC","items.price":{"$gt":100}} matches a
	// document where one element has the sku and a *different* element has the
	// price — the caller asked for one item that is both. Declaring the array
	// makes the generator group sibling AND predicates into a single
	// {"items":{"$elemMatch":{…}}}, which is the same-element reading.
	//
	// Leave it empty for a plain embedded document (address.city): there is only
	// ever one sub-document there, so dot notation is already exact.
	ElemMatch string `json:"elemMatch,omitempty"`

	// ValueCase forces the letter case of the string values this field compiles
	// into the final query: "upper", "lower", or "" (leave exactly as written).
	//
	// It exists for the common mismatch where the stored column does not use the
	// same case as the words people say. A status column holding "SHIPPED" will
	// not match the "shipped" the user typed, and teaching the model to shout is
	// both unreliable and pointless — the case is a property of the storage, not
	// of the question. Declaring it here keeps the config's `values` as the one
	// spoken vocabulary (the model still sees and must emit those exact strings,
	// and validation still checks them verbatim) while the generator writes the
	// physical form. The conversion is therefore invisible to the model and
	// happens only on the way out, per backend.
	//
	// Only string-valued fields may set it: string, enum, and array whose
	// itemType is one of those. Numbers, booleans and dates have no case, and
	// silently ignoring the flag on them would hide a config mistake, so load
	// rejects it there. Raw `regex` values are also left untouched — recasing a
	// pattern would turn \d into \D and invert its meaning.
	ValueCase ValueCase `json:"valueCase,omitempty"`

	// --- Capability flags: what operations this field may take part in. ---
	Queryable  *bool `json:"queryable,omitempty"`  // include/exclude from the NLP surface; false = hidden + rejected if referenced (default true)
	Filterable *bool `json:"filterable,omitempty"` // may appear in structured filter predicates (default true)
	Searchable *bool `json:"searchable,omitempty"` // may use text-search operators (contains/startsWith/endsWith/regex) (default: true for strings, else false)
	Sortable   *bool `json:"sortable,omitempty"`   // may appear in sort[] (default: false for arrays, else true)
	Returnable *bool `json:"returnable,omitempty"` // may appear in the result projection/select (default true)

	// --- Hints: do not gate anything, but steer ordering and the prompt. ---
	Indexed  bool `json:"indexed,omitempty"`  // field is backed by a DB index; used for predicate ordering + soft warnings
	Priority int  `json:"priority,omitempty"` // relative importance; higher sorts earlier in predicates and prompt

	Validators *FieldValidators `json:"validators,omitempty"` // deterministic value constraints (numeric bounds)
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

// Policy encodes deterministic guardrails. Every field here is enforced during
// validation — there are no reserved or aspirational keys, because a guardrail
// that parses but does nothing is worse than one that does not exist: it reads
// as protection in review and provides none at runtime.
//
// Multi-tenancy is deliberately absent. It is not a config key but a per-call
// argument: pass a Scope to Translate/GenerateFrom and the tenant predicate is
// AND-ed onto the root of the filter tree after validation, where no model
// output can widen or negate it. See scope.go.
type Policy struct {
	MaxNestingDepth int      `json:"maxNestingDepth,omitempty"`
	DenyRegexOn     []string `json:"denyRegexOn,omitempty"`
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

	// Check every model block's apiKeyEnv before anything else can go wrong.
	// The field holds the NAME of an environment variable, never a key, and
	// pasting the key here is the natural mistake: the resulting os.Getenv
	// lookup silently returns "", no Authorization header is sent, and the
	// provider answers with a generic 400 that says nothing about the cause.
	if err := c.Model.validateAPIKeyEnv("model"); err != nil {
		return err
	}
	for i := range c.Models {
		if err := c.Models[i].validateAPIKeyEnv(fmt.Sprintf("models[%d]", i)); err != nil {
			return err
		}
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
		// The value-case rule is rejected rather than ignored when it cannot
		// apply, because both mistakes it catches are invisible at runtime: a
		// misspelt setting and a case rule on a field that has no letters would
		// each leave the query silently uncased.
		if !validValueCase(f.ValueCase) {
			return fmt.Errorf("config: field %q has invalid valueCase %q (allowed: %q, %q, or omit the key)",
				f.Name, f.ValueCase, CaseLower, CaseUpper)
		}
		if f.ValueCase != CaseAsIs && !f.carriesStrings() {
			return fmt.Errorf("config: field %q declares valueCase %q but its values are not strings (type %q) — "+
				"valueCase applies to string and enum fields, or arrays of them",
				f.Name, f.ValueCase, f.Type)
		}
		// Nested Mongo paths: a malformed dot path or an elemMatch that does not
		// contain the field yields a query matching nothing, so reject it here.
		if err := c.validateMongoPaths(f); err != nil {
			return err
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

// MongoElemMatch reports whether the named field lives inside an array of
// sub-documents and, when it does, returns the array's path plus the field's
// path *relative* to one array element.
//
// For a field mapped to "items.sku" with elemMatch "items" it returns
// ("items", "sku", true) — the two halves the $elemMatch document needs. The
// relative path keeps its own dots, so "items.dims.w" under "items" yields
// "dims.w" and still addresses the right leaf inside the element.
func (c *Config) MongoElemMatch(fieldName string) (arrayPath, relPath string, ok bool) {
	f, found := c.fieldByName[fieldName]
	if !found || f.ElemMatch == "" { // unregistered or a plain (non-array) path
		return "", "", false
	}
	full := c.PhysicalName(fieldName, "mongo") // the complete dot path
	rel := strings.TrimPrefix(full, f.ElemMatch+".")
	if rel == full || rel == "" { // finalize guarantees the prefix; belt and braces
		return "", "", false
	}
	return f.ElemMatch, rel, true
}

// validDotPath reports whether s is a usable Mongo field path: one or more
// non-empty, dot-separated segments, none of which starts with "$" (reserved
// for operators) or contains a NUL. A typo like "items..sku" or a stray leading
// dot would otherwise become a filter key that matches nothing, with no error
// anywhere to explain the empty result set.
func validDotPath(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" { // leading, trailing, or doubled dot
			return false
		}
		if strings.HasPrefix(seg, "$") { // would be read as an operator
			return false
		}
		if strings.ContainsRune(seg, 0) { // NUL is illegal in a BSON key
			return false
		}
	}
	return true
}

// validateMongoPaths checks the field's Mongo dot path and its elemMatch
// declaration against each other, so a mismatch fails at load rather than
// producing a filter that quietly matches nothing.
func (c *Config) validateMongoPaths(f *Field) error {
	full := f.Name // the path used when no mongo mapping is declared
	if m, ok := f.Mapping["mongo"]; ok && m != "" {
		full = m
	}
	if !validDotPath(full) {
		return fmt.Errorf("config: field %q has an invalid mongo path %q "+
			"(expected dot-separated segments, e.g. \"items.sku\"; no empty segments, no leading \"$\")", f.Name, full)
	}
	if f.ElemMatch == "" { // nothing further to check for a plain path
		return nil
	}
	if !validDotPath(f.ElemMatch) {
		return fmt.Errorf("config: field %q has an invalid elemMatch path %q "+
			"(expected the dot path of the array, e.g. \"items\")", f.Name, f.ElemMatch)
	}
	// The array path must actually contain the field, otherwise the generated
	// $elemMatch would search the wrong sub-document.
	if rel := strings.TrimPrefix(full, f.ElemMatch+"."); rel == full || rel == "" {
		return fmt.Errorf("config: field %q declares elemMatch %q but its mongo path is %q — "+
			"the mongo path must sit inside that array (e.g. elemMatch \"items\" with mongo \"items.sku\")",
			f.Name, f.ElemMatch, full)
	}
	return nil
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
