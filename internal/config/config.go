// Package config holds the schema every other package reads: the entity, its
// fields and their types, the per-backend physical mapping, the model block, and
// the policy limits.
//
// Three source files make one package because Go requires it — elastic_config.go
// and valuecase.go both declare methods on Config and Field, and a method has to
// share a package with its receiver.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/identifier"
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

	// Protocol names the wire dialect explicitly: "openai" or "anthropic".
	// Usually omitted — a known provider name implies it, and anything else
	// defaults to the OpenAI dialect that nearly every endpoint speaks.
	//
	// Set it when the endpoint's URL does not advertise its dialect: an
	// Anthropic-compatible corporate gateway, a proxy, or any host whose name
	// would otherwise be guessed wrongly. Explicit always wins over inference.
	Protocol string `json:"protocol,omitempty"`

	// TimeoutSeconds bounds ONE request to the provider. Default 30.
	// Per-attempt, like the HTTP client timeout it replaces; the total bound on
	// a translate is the caller's context.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// MaxRetries is how many EXTRA attempts a retryable failure gets — a rate
	// limit, a 5xx, a dropped connection. Default 2; set 0 to disable.
	//
	// Only failures that time might fix are retried. A bad key, an unknown
	// model or a malformed request fail immediately, because the next attempt
	// would send exactly the same thing.
	//
	// A pointer so that an explicit 0 ("never retry") is distinguishable from
	// an absent key ("use the default").
	MaxRetries *int `json:"maxRetries,omitempty"`

	// RetryBackoffMs is the first retry delay in milliseconds, doubling each
	// retry and jittered. Default 250. A provider's own Retry-After header
	// overrides this whenever it sends one.
	RetryBackoffMs int `json:"retryBackoffMs,omitempty"`

	// JSONMode opts into the OpenAI response_format=json_object flag. It is off
	// by default because it is not uniformly implemented: Gemini's
	// OpenAI-compatibility endpoint returns brace-unbalanced JSON with it set
	// (measured: 2/5 replies parseable with it, 5/5 without), while the prompt
	// already demands a bare JSON object and the parser tolerates code fences.
	// Turn it on for endpoints where it genuinely helps.
	JSONMode *bool `json:"jsonMode,omitempty"`
}

// Default retry/timeout settings. These are the values a model block gets when
// it says nothing, and they are chosen to be safe for the case QueryForge is
// most often pointed at: a free-tier key, where HTTP 429 is routine rather than
// exceptional.
//
// They are exported so internal/provider can read them: the retry loop that
// obeys them lives there, but the Effective* accessors that apply the defaults
// are methods on ModelConfig and so have to be declared here.
const (
	// DefaultTimeout bounds ONE round trip, matching http.Client.Timeout, which
	// is what this replaces. It is per-attempt rather than per-Complete because
	// the honest total bound is the caller's context: the SDKs already set one,
	// and a second total budget hidden in the model block would silently
	// contradict it.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the number of ADDITIONAL attempts after the first,
	// so the default is up to three round trips.
	//
	// Two, not zero: a rate-limited free tier recovers within a second or two,
	// and failing that call outright wastes a translation the user is waiting
	// on. Two, not more: every retry is spending someone's quota, and a chain
	// of `models` fallbacks multiplies this — three models at two retries is
	// already nine requests for one question.
	DefaultMaxRetries = 2

	// DefaultRetryBackoff is the first delay; each further retry doubles it.
	DefaultRetryBackoff = 250 * time.Millisecond
)

// EffectiveJSONMode reports whether to send response_format=json_object.
// Defaults to false — see JSONMode.
func (m ModelConfig) EffectiveJSONMode() bool { return m.JSONMode != nil && *m.JSONMode }

// EffectiveTimeout returns the per-request timeout, defaulting to 30s. A
// negative value is treated as unset rather than as an instant deadline: a
// timeout of "-5" is a typo, and honouring it literally would make every call
// fail before it started, with an error that points nowhere near the config.
func (m ModelConfig) EffectiveTimeout() time.Duration {
	if m.TimeoutSeconds > 0 {
		return time.Duration(m.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

// EffectiveMaxRetries returns the retry budget, defaulting to 2. An explicit 0
// disables retrying; a negative value is clamped to 0.
func (m ModelConfig) EffectiveMaxRetries() int {
	if m.MaxRetries == nil {
		return DefaultMaxRetries
	}
	if *m.MaxRetries < 0 {
		return 0
	}
	return *m.MaxRetries
}

// EffectiveRetryBackoff returns the base retry delay, defaulting to 250ms.
func (m ModelConfig) EffectiveRetryBackoff() time.Duration {
	if m.RetryBackoffMs > 0 {
		return time.Duration(m.RetryBackoffMs) * time.Millisecond
	}
	return DefaultRetryBackoff
}

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

// validateModelBlock runs every self-check on one model block. Grouped into a
// single entry point so the primary `model` and each `models[i]` fallback are
// held to identical rules — a fallback that only fails when the primary is
// already down is the worst possible time to discover a typo.
func (m ModelConfig) validateModelBlock(where string) error {
	if err := m.validateAPIKeyEnv(where); err != nil {
		return err
	}
	return m.validateProtocol(where)
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

// IsEmptyModel reports whether the block names no endpoint at all — used to skip
// a blank primary Model when only the Models fallback list is populated.
//
// A package-level function rather than a method for the reason given on
// ValueCaseFor: ModelConfig is re-exported as queryforge.ModelConfig, and a new
// exported method would widen the public API.
func IsEmptyModel(m ModelConfig) bool {
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

	// --- Elasticsearch/OpenSearch-only source modes. At most one of Index,
	// Indexes, Alias, and Routing may be set; none set falls back to the
	// entity name as a direct index, matching every other backend's fallback.

	// Indexes lists several concrete physical indexes to search at once
	// (Elasticsearch/OpenSearch "MULTIPLE_INDEX"), e.g.
	// ["orders-2025","orders-2026"], rendered as a comma-joined path segment.
	Indexes []string `json:"indexes,omitempty"`

	// Alias names an Elasticsearch/OpenSearch alias to search. It resolves to
	// the identical request shape as Index (GET /<name>/_search) — ES itself
	// does not distinguish an alias from a concrete index at that URL — but is
	// tracked as its own source mode so a caller introspecting the resolved
	// query can still tell "this is an alias" from "this is a concrete index".
	Alias string `json:"alias,omitempty"`

	// Routing configures business-rule index resolution: which physical
	// index/indexes a query hits is derived deterministically from the
	// query's own (config-declared) routing-field value, rather than fixed in
	// config. See IndexRouting.
	Routing *IndexRouting `json:"routing,omitempty"`
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

// SourceType names how a backend's physical index/indexes were determined.
// Elasticsearch/OpenSearch only — every other backend has exactly one
// physical source and no notion of "how it was chosen".
type SourceType string

const (
	SourceDirectIndex   SourceType = "DIRECT_INDEX"
	SourceMultipleIndex SourceType = "MULTIPLE_INDEX"
	SourceAlias         SourceType = "ALIAS"
	SourceBusinessRule  SourceType = "BUSINESS_RULE_INDEX"
)

// elasticSourceCount reports how many of the mutually exclusive ES source
// modes this backend config sets, for the "at most one" load-time check.
func (b BackendConfig) elasticSourceCount() int {
	n := 0
	if b.Index != "" {
		n++
	}
	if len(b.Indexes) > 0 {
		n++
	}
	if b.Alias != "" {
		n++
	}
	if b.Routing != nil {
		n++
	}
	return n
}

// IndexRoutingStrategy selects how IndexRouting resolves physical indexes
// from a query.
type IndexRoutingStrategy string

const (
	RoutingPattern IndexRoutingStrategy = "pattern"
	RoutingDate    IndexRoutingStrategy = "date"
	RoutingRules   IndexRoutingStrategy = "rules"
	RoutingIfElse  IndexRoutingStrategy = "ifElse"
)

// DateGranularity is the partition size a "date" routing strategy resolves
// to, and the format of its IndexPattern token: YEAR needs "{yyyy}", MONTH
// needs "{yyyy-MM}", DAY needs "{yyyy-MM-dd}".
type DateGranularity string

const (
	GranularityYear  DateGranularity = "YEAR"
	GranularityMonth DateGranularity = "MONTH"
	GranularityDay   DateGranularity = "DAY"
)

// IndexRouting configures Elasticsearch/OpenSearch business-rule index
// resolution. It is entirely config, never query: the query only ever
// supplies the VALUE of a field this config has explicitly opted into
// routing (Field.RoutingField); every index name it can possibly resolve to
// — Default, a rule's Indexes, a branch's Indexes, or IndexPattern's literal
// text — is written here by whoever owns this config. See the package-level
// security note in source_es.go for why that separation matters.
type IndexRouting struct {
	Strategy IndexRoutingStrategy `json:"strategy"`

	// Field is the routing field driving resolution. Required for "pattern"
	// and "date"; "rules" and "ifElse" instead name a field on each
	// condition, since different rules/branches may key off different
	// fields.
	Field string `json:"field,omitempty"`

	// IndexPattern is a physical index name template containing exactly one
	// "{token}" placeholder, e.g. "orders-{yyyy}" or "tenant-{tenantId}-orders".
	// Required for "pattern" and "date"; for "date" the token must match
	// Granularity exactly ("{yyyy}"/"{yyyy-MM}"/"{yyyy-MM-dd}").
	IndexPattern string `json:"indexPattern,omitempty"`

	// Granularity is the partition size for "date" routing.
	Granularity DateGranularity `json:"granularity,omitempty"`

	// Rules is the rule list for "rules" routing. Every rule whose condition
	// the query's known field value satisfies is a candidate; ties at the
	// highest Priority are rejected as ambiguous rather than guessed at.
	Rules []IndexRule `json:"rules,omitempty"`

	// Branches is the ordered branch list for "ifElse" routing. The first
	// branch whose condition matches wins. A branch may set Else:true instead
	// of If to match unconditionally; at most one branch may do so, and it
	// must be the last.
	Branches []IndexBranch `json:"branches,omitempty"`

	// Default is the index (or indexes) to search when the routing field is
	// absent from the query, its value cannot be resolved to a single
	// comparable literal, or (for "rules"/"ifElse") no rule/branch matches.
	// Required — QueryForge never silently falls back to an unrestricted
	// wildcard, so an unconfigured fallback means the query is rejected
	// rather than searching every index.
	Default []string `json:"default,omitempty"`
}

// RoutingCondition is one equality/range test used by "rules" and "ifElse"
// routing. It is deliberately narrower than the full Operator catalogue:
// routing picks WHERE to search, and only a single comparable literal — from
// an equals/gt/gte/lt/lte/before/after predicate reachable through AND alone
// — is ever sound to route on. See source_es.go.
type RoutingCondition struct {
	Field    string       `json:"field"`
	Operator ast.Operator `json:"operator"` // equals, notEquals, gt, gte, lt, lte only
	Value    string       `json:"value"`    // literal to compare against, in its field's textual form
}

// IndexRule is one "rules"-strategy entry: When the condition holds, search
// Indexes. Priority breaks a tie when more than one rule matches the same
// query; equal top priority on multiple matches is a runtime resolution
// error rather than a guess.
type IndexRule struct {
	When     RoutingCondition `json:"when"`
	Indexes  []string         `json:"indexes"`
	Priority int              `json:"priority,omitempty"`
}

// IndexBranch is one "ifElse"-strategy entry, evaluated in order. Else:true
// branches match unconditionally and carry no If.
type IndexBranch struct {
	If      *RoutingCondition `json:"if,omitempty"`
	Else    bool              `json:"else,omitempty"`
	Indexes []string          `json:"indexes"`
}

// isElasticBackend reports whether id names one of the Elasticsearch-family
// backends, which share the source-resolution and mapping rules in this file
// (Indexes/Alias/Routing, KeywordMapping, NestedPath) that no other backend
// uses.
func isElasticBackend(id string) bool {
	return id == "elasticsearch" || id == "opensearch"
}

// IsSQLBackend reports whether a backend id names one of the SQL dialects, whose
// physical names are written into statement text and are therefore held to the
// identifier rule at config load.
//
// It lives here, spelled with the same literals isElasticBackend uses, because
// config validation is its first caller and a config package cannot import the
// generator package that reads it back. The ids match sqlDialect.backend for the
// Postgres and MySQL dialects exactly.
func IsSQLBackend(id string) bool {
	return id == "sql" || id == "mysql"
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
	Name      string         `json:"name"`                // logical field name used in the AST
	Type      FieldType      `json:"type"`                // logical type; bounds legal operators and value kinds
	Values    []string       `json:"values,omitempty"`    // enum domain (required when type == enum)
	ItemType  FieldType      `json:"itemType,omitempty"`  // element type for array fields
	Operators []ast.Operator `json:"operators,omitempty"` // explicit comparison-operator whitelist; empty = type defaults
	Synonyms  []string       `json:"synonyms,omitempty"`  // alternate phrasings that resolve to this field

	// --- LLM-facing metadata: what the model (and Explain's prose readback) is
	// told about this field, beyond its name and type. All optional on an
	// ordinary field. CustomField makes Description — and, on a searchable
	// string field, ValueHint — mandatory, because a generically- or
	// per-tenant-named field (e.g. "txt01") gives the model nothing to go on
	// without it. See validateFieldMetadata. ---

	// CustomField marks a field whose logical name does not explain itself.
	// It changes nothing about how the field is queried or compiled; it only
	// tightens config-load validation so such a field cannot ship without real
	// context for the model.
	CustomField bool `json:"customField,omitempty"`

	// DisplayName is a human label shown to the model (alongside, never instead
	// of, the field's logical Name — the model still must emit Name in the AST)
	// and used by Explain's prose readback in place of the raw field name.
	// Always optional, even on a custom field: falls back to Name when empty.
	DisplayName string `json:"displayName,omitempty"`

	// Description is a one-line note on what the field means, shown to the
	// model in the prompt. Optional on an ordinary field; REQUIRED when
	// CustomField is true.
	Description string `json:"description,omitempty"`

	// ValueHint describes what a free-text field typically contains, since the
	// model cannot see real values the way it can an enum's Values. Legal only
	// on a searchable string field (type "string" with EffectiveSearchable()
	// true) — load rejects it elsewhere, the same restriction ValueCase and
	// KeywordMapping already apply to fields their meaning cannot reach.
	// Optional on an ordinary searchable string field; REQUIRED when
	// CustomField is true on one.
	ValueHint string `json:"valueHint,omitempty"`

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

	// CaseInsensitive makes every string-comparison operator on this field match
	// regardless of letter case, on every backend: equals, notEquals, in, notIn,
	// contains, startsWith, endsWith.
	//
	// It exists for the mismatch valueCase cannot fix: a free-text column where
	// the stored case is not one consistent case at all — "Black", "BLACK" and
	// "black" can all be sitting in the same column, or the model's guess just
	// does not match whatever a particular row happens to hold. valueCase folds
	// the query's value to a fixed case on the assumption the storage is
	// consistent; this instead makes the comparison itself blind to case, so it
	// works whichever case the row is in. The two solve the same symptom from
	// opposite ends and are mutually exclusive per field — load rejects a field
	// that sets both.
	//
	// Only a plain string field may set it (not enum, not array, not date/number/
	// boolean): an enum's fix is to declare `values` in the storage's exact case,
	// and folding a pattern field like regex would change what the pattern means.
	//
	// The cost is the one every case-insensitive comparison has: on SQL it is
	// rendered as LOWER(column) <op> LOWER(?), which cannot use a plain index on
	// that column unless a matching expression index exists. On Mongo it compiles
	// equals/notEquals/in/notIn to anchored `/^…$/i` patterns rather than exact
	// matches. contains is unaffected on Mongo, which already matches
	// case-insensitively there regardless of this flag.
	CaseInsensitive bool `json:"caseInsensitive,omitempty"`

	// KeywordMapping declares, per Elasticsearch/OpenSearch-family backend,
	// the exact-match "keyword" sub-field backing this logical field — the
	// standard ES convention of indexing one string twice (an analyzed "text"
	// path for full-text search, plus an unanalyzed "keyword" path for exact
	// match, sort, and aggregation) because a single physical field cannot do
	// both.
	//
	// Mapping[backend] is read as the text path; equals/notEquals/in/notIn,
	// sort, and any future aggregation instead prefer KeywordMapping[backend]
	// when present, falling back to Mapping[backend] (then the logical name)
	// when it is not. Only `contains` ever reads the text path.
	//
	// Only legal on a field whose values are strings (string, enum, or an
	// array of them) — load rejects it elsewhere, the same rule ValueCase
	// uses and for the same reason: a number or date has no text/keyword
	// split to declare.
	KeywordMapping map[string]string `json:"keywordMapping,omitempty"`

	// NestedPath names the Elasticsearch/OpenSearch "nested" object/array this
	// field lives inside, as a dot path from the document root (e.g. "items"
	// for a field mapped to "items.sku"). Elasticsearch/OpenSearch-only —
	// Mongo's equivalent problem is solved by ElemMatch, kept as a fully
	// separate field so the two backends' nested/array semantics never share
	// validation or generation code.
	//
	// Without it, sibling predicates on an ES "nested" field would each be
	// wrapped in their own independent nested query and could each match a
	// DIFFERENT array element — the same "sku on item #1, price on item #7"
	// bug ElemMatch prevents for Mongo. Declaring it makes the generator fold
	// sibling AND predicates on the same path into one nested query instead.
	//
	// Leave it empty for an ES "object" (not "nested") field, where dot
	// notation is already exact.
	NestedPath string `json:"nestedPath,omitempty"`

	// RoutingField opts this field into Elasticsearch/OpenSearch business-rule
	// index resolution (backends.elasticsearch.routing.field, or a
	// rules/ifElse condition's field). It is deliberately separate from
	// Filterable: an ordinary filterable field does not thereby select which
	// physical index is searched, and QueryForge does not guess from a
	// field's type (e.g. date) that it is meant to shard the index — the
	// consumer opts a field in explicitly.
	RoutingField bool `json:"routingField,omitempty"`

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

	// AllowRegexOn turns regex into an opt-IN capability. When it lists at least
	// one field, regex is legal on those fields and on no others; DenyRegexOn
	// still applies on top, so a field named by both is denied.
	//
	// Both exist because they answer different questions. DenyRegexOn is the
	// original, and it is an enumerated deny-list: a config that simply does not
	// mention a field leaves regex enabled on it. That is the wrong default for a
	// capability whose cost is paid by the database server — `(a+)+$` against a
	// text column is exponential work inside the query, reachable from an
	// ordinary English sentence. A new config should set AllowRegexOn (or leave
	// regex out of every field's `operators` list); DenyRegexOn is kept because
	// removing it would silently widen every config that relies on it.
	AllowRegexOn []string `json:"allowRegexOn,omitempty"`

	// --- Request-size bounds. Zero means "use the built-in default". ---
	//
	// These bound the WORK one AST can cause, which maxNestingDepth does not:
	// depth says nothing about breadth, and a filter can be flat, enormous, and
	// entirely legal. They matter most for a service that accepts AST JSON
	// directly (queryforge_service does), where the AST is caller input rather
	// than model output.
	MaxFilterNodes  int `json:"maxFilterNodes,omitempty"`  // total condition nodes in the filter tree
	MaxListLength   int `json:"maxListLength,omitempty"`   // elements in an in/notIn/containsAny/containsAll value
	MaxValueLength  int `json:"maxValueLength,omitempty"`  // characters in a string value
	MaxRegexLength  int `json:"maxRegexLength,omitempty"`  // characters in a regex pattern
	MaxSuggestCalls int `json:"maxSuggestCalls,omitempty"` // unknown fields that get "did you mean" suggestions

	// Requires declares cross-field business rules: "when field A is filtered,
	// field B must be filtered too" (e.g. passport expiry needs a country to be
	// meaningful). Structurally legal ASTs can still break one of these, which
	// is why they are checked separately from everything else in this struct —
	// see PolicyViolationError. Empty (the default) means no such rule exists,
	// and every AST that passes ordinary validation is accepted, unchanged from
	// before this field existed.
	Requires []FieldRequirement `json:"requires,omitempty"`
}

// FieldRequirement is one business rule: filtering When.Field (optionally only
// with one of When.Operators) makes the query meaningless unless the question
// also filters at least one field in RequireAlsoOneOf, anywhere in the filter
// tree. See PolicyViolationError for how a broken rule is reported.
type FieldRequirement struct {
	When             RequirementTrigger `json:"when"`
	RequireAlsoOneOf []string           `json:"requireAlsoOneOf"`
	Message          string             `json:"message,omitempty"` // shown instead of the generated default when set
}

// RequirementTrigger names the field (and, optionally, which operators) that
// switches a FieldRequirement on. Operators empty means "any operator on this
// field triggers the rule".
type RequirementTrigger struct {
	Field     string   `json:"field"`
	Operators []string `json:"operators,omitempty"`
}

// Built-in policy ceilings, applied when the config leaves the key at zero.
//
// They are generous enough that no honest question reaches them — a 200-element
// IN list is already a report, not a sentence — and small enough that a hostile
// or runaway AST cannot turn one request into unbounded work. A config may
// raise them; setting a negative value disables the bound entirely.
const (
	DefaultMaxFilterNodes  = 500
	DefaultMaxListLength   = 500
	DefaultMaxValueLength  = 4096
	DefaultMaxRegexLength  = 256
	DefaultMaxSuggestCalls = 10
)

// Length ceilings on the LLM-facing metadata strings (DisplayName/Description/
// ValueHint), applied at config load. They exist so one field's config cannot
// silently bloat every prompt this config ever sends — a config author who
// wants more than a couple of sentences here almost certainly means a summary
// of the DATA, which is ValueHint's job, not a hand-written essay repeated on
// every translate call.
const (
	MaxDisplayNameLength = 200
	MaxDescriptionLength = 500
	MaxValueHintLength   = 1000
)

// limitOr returns the configured bound, the default when unset, or 0 (meaning
// "no bound") when the config explicitly set a negative value.
func limitOr(configured, def int) int {
	switch {
	case configured > 0:
		return configured
	case configured < 0:
		return 0 // explicitly disabled
	default:
		return def
	}
}

// EffectiveMaxFilterNodes and friends resolve each bound for this config. The
// validator in internal/validate is the caller, which is why they are exported —
// and why they are package-level functions rather than methods: Policy is
// re-exported verbatim as queryforge.Policy, and methods added here would land in
// the library's public API.
func EffectiveMaxFilterNodes(p Policy) int {
	return limitOr(p.MaxFilterNodes, DefaultMaxFilterNodes)
}
func EffectiveMaxListLength(p Policy) int { return limitOr(p.MaxListLength, DefaultMaxListLength) }
func EffectiveMaxValueLength(p Policy) int {
	return limitOr(p.MaxValueLength, DefaultMaxValueLength)
}
func EffectiveMaxRegexLength(p Policy) int {
	return limitOr(p.MaxRegexLength, DefaultMaxRegexLength)
}
func EffectiveMaxSuggestCalls(p Policy) int {
	return limitOr(p.MaxSuggestCalls, DefaultMaxSuggestCalls)
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

// Finalize runs the load-time step — index building and integrity validation —
// on a Config that was built in Go rather than parsed, which ParseConfig would
// otherwise be the only way to reach.
//
// It exists for the regression tests that construct a hostile Config directly to
// prove the generators defend themselves against one. A package-level function,
// not a method, so that Config's public API (it is re-exported verbatim as
// queryforge.Config) does not grow one.
func Finalize(c *Config) error { return c.finalize() }

// FieldIndex exposes the by-name field index Finalize builds. Same audience and
// the same reasoning: a test that has to corrupt a resolved field in place
// cannot reach the unexported map from another package.
func FieldIndex(c *Config) map[string]*Field { return c.fieldByName }

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
	if err := c.Model.validateModelBlock("model"); err != nil {
		return err
	}
	for i := range c.Models {
		if err := c.Models[i].validateModelBlock(fmt.Sprintf("models[%d]", i)); err != nil {
			return err
		}
	}
	// A SQL backend's table name is written into the emitted statement as
	// syntax, never bound, so it has to be an identifier. Checking here means a
	// typo (or anything worse) fails at load with the backend named, instead of
	// at the database with a parse error nobody can trace back.
	//
	// Only the SQL dialects are checked. A Mongo collection or an Elasticsearch
	// index is a parameter, not syntax — "orders-v2" is a perfectly ordinary
	// index name — so imposing SQL's identifier rule on them would reject valid
	// configs to prevent a problem those backends do not have.
	for backend, bc := range c.Backends {
		if !IsSQLBackend(backend) {
			continue
		}
		if src := bc.Source(); src != "" && !identifier.ValidIdentPath(src) {
			return fmt.Errorf("config: backends.%s source %q is not a valid SQL identifier "+
				"(expected dot-separated letters, digits and underscores, not starting with a digit)", backend, src)
		}
	}

	c.fieldByName = make(map[string]*Field, len(c.Fields))
	c.synonymIndex = make(map[string]*Field)
	for i := range c.Fields {
		f := &c.Fields[i]
		if f.Name == "" {
			return fmt.Errorf("config: field #%d has no name", i)
		}
		// An explicit SQL mapping becomes a column name in the statement text, so
		// it is held to the identifier rule at load. The logical name is not
		// checked here — it is the fallback physical name for every backend, and a
		// Mongo-only config may legitimately use a name SQL would need quoting for.
		// The SQL generators check the name they are actually about to emit
		// (sqlIdent), so that case fails where it really is a problem, and only there.
		for backend, phys := range f.Mapping {
			if phys != "" && IsSQLBackend(backend) && !identifier.ValidIdentPath(phys) {
				return fmt.Errorf("config: field %q has an invalid %s mapping %q "+
					"(expected dot-separated letters, digits and underscores, not starting with a digit)", f.Name, backend, phys)
			}
		}
		if !ValidFieldType(f.Type) {
			return fmt.Errorf("config: field %q has invalid type %q", f.Name, f.Type)
		}
		if f.Type == FieldEnum && len(f.Values) == 0 {
			return fmt.Errorf("config: enum field %q must list values", f.Name)
		}
		for _, op := range f.Operators {
			if !IsKnownOperator(op) {
				return fmt.Errorf("config: field %q lists unknown operator %q", f.Name, op)
			}
		}
		if err := validateFieldMetadata(f); err != nil {
			return err
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
		// caseInsensitive is narrower than valueCase on purpose: an enum's exact
		// case belongs in `values`, and folding a regex pattern would change what
		// it matches, so only a plain string field may set the flag.
		if f.CaseInsensitive && f.Type != FieldString {
			return fmt.Errorf("config: field %q sets caseInsensitive but is type %q — "+
				"caseInsensitive applies to string fields only (an enum's case belongs in its values list)",
				f.Name, f.Type)
		}
		if f.CaseInsensitive && f.ValueCase != CaseAsIs {
			return fmt.Errorf("config: field %q sets both caseInsensitive and valueCase %q — "+
				"they solve the same mismatch from opposite ends and cannot both apply: "+
				"valueCase forces one case, caseInsensitive matches any case",
				f.Name, f.ValueCase)
		}
		// KeywordMapping is the ES/OpenSearch analogue of ValueCase's own
		// restriction: it describes a second physical representation of a
		// string value, so it means nothing on a field that carries no
		// strings.
		if len(f.KeywordMapping) > 0 && !f.carriesStrings() {
			return fmt.Errorf("config: field %q declares keywordMapping but its values are not strings (type %q) — "+
				"keywordMapping applies to string and enum fields, or arrays of them", f.Name, f.Type)
		}
		for backend, phys := range f.KeywordMapping {
			if phys != "" && !validDotPath(phys) {
				return fmt.Errorf("config: field %q has an invalid %s keywordMapping %q "+
					"(expected dot-separated segments, e.g. \"customerName.keyword\")", f.Name, backend, phys)
			}
		}
		// Nested Mongo paths: a malformed dot path or an elemMatch that does not
		// contain the field yields a query matching nothing, so reject it here.
		if err := c.validateMongoPaths(f); err != nil {
			return err
		}
		// Nested Elasticsearch/OpenSearch paths: the same shape of check, kept
		// fully separate from Mongo's (see NestedPath's doc comment).
		if err := c.validateElasticPaths(f); err != nil {
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

	// Business-rule index routing references fields by name, so it can only be
	// checked once fieldByName is built.
	for backend, bc := range c.Backends {
		if !isElasticBackend(backend) {
			continue
		}
		if err := c.validateElasticSource(backend, bc); err != nil {
			return err
		}
	}

	// Cross-field business rules reference fields by name too, so they can only
	// be checked once fieldByName is built.
	if err := c.validatePolicyRequires(); err != nil {
		return err
	}
	return nil
}

// validatePolicyRequires checks every policy.requires rule: the triggering
// field and every field it requires alongside it must be registered, and each
// listed operator must be one this library knows. Catching this at load time,
// the same way IndexRouting.RoutingField is, means a typo in a rule fails
// loudly when the config is loaded rather than silently never firing.
func (c *Config) validatePolicyRequires() error {
	for i, r := range c.Policy.Requires {
		where := fmt.Sprintf("policy.requires[%d]", i)
		if r.When.Field == "" {
			return fmt.Errorf("config: %s.when.field is required", where)
		}
		if _, ok := c.fieldByName[r.When.Field]; !ok {
			return fmt.Errorf("config: %s.when references field %q, which is not registered", where, r.When.Field)
		}
		for _, op := range r.When.Operators {
			if !IsKnownOperator(ast.Operator(op)) {
				return fmt.Errorf("config: %s.when.operators lists unknown operator %q", where, op)
			}
		}
		if len(r.RequireAlsoOneOf) == 0 {
			return fmt.Errorf("config: %s.requireAlsoOneOf must list at least one field", where)
		}
		for _, name := range r.RequireAlsoOneOf {
			if _, ok := c.fieldByName[name]; !ok {
				return fmt.Errorf("config: %s.requireAlsoOneOf references field %q, which is not registered", where, name)
			}
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

// validateFieldMetadata enforces the LLM-facing metadata rules: length
// ceilings on every field, ValueHint's restriction to searchable string
// fields, and CustomField's requirement that a field whose name does not
// explain itself still carries real context. Checked at load so a broken rule
// fails here, not as a silently useless (or missing) prompt line at runtime.
func validateFieldMetadata(f *Field) error {
	if len(f.DisplayName) > MaxDisplayNameLength {
		return fmt.Errorf("config: field %q displayName is %d characters, over the %d-character limit",
			f.Name, len(f.DisplayName), MaxDisplayNameLength)
	}
	if len(f.Description) > MaxDescriptionLength {
		return fmt.Errorf("config: field %q description is %d characters, over the %d-character limit",
			f.Name, len(f.Description), MaxDescriptionLength)
	}
	if len(f.ValueHint) > MaxValueHintLength {
		return fmt.Errorf("config: field %q valueHint is %d characters, over the %d-character limit",
			f.Name, len(f.ValueHint), MaxValueHintLength)
	}

	// freeText is the only shape of field a valueHint can describe: a string
	// field whose search operators (contains/regex/…) are actually enabled.
	// A non-searchable string has no free-text operator to give the model
	// context for, and no other type carries unstructured text at all.
	freeText := f.Type == FieldString && f.EffectiveSearchable()
	if f.ValueHint != "" && !freeText {
		return fmt.Errorf("config: field %q declares valueHint but is not a searchable string field (type %q) — "+
			"valueHint describes free-text content the model cannot otherwise see, so it only applies there",
			f.Name, f.Type)
	}

	if f.CustomField {
		// A label is not context: displayName alone (or nothing at all) leaves
		// the model guessing what a generically-named field like "txt01" means.
		if strings.TrimSpace(f.Description) == "" {
			return fmt.Errorf("config: field %q is marked customField but has no description — "+
				"a generically- or per-tenant-named field needs explicit context for the model to use it correctly",
				f.Name)
		}
		// An enum customField's equivalent need — a defined domain — is already
		// covered by the pre-existing "enum must list values" check above, so
		// no separate rule is needed here for that case.
		if freeText && strings.TrimSpace(f.ValueHint) == "" {
			return fmt.Errorf("config: field %q is marked customField and is a searchable free-text field, "+
				"but has no valueHint — describe what this field typically contains so the model can use it",
				f.Name)
		}
	}
	return nil
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
func (f Field) EffectiveOperators() []ast.Operator {
	if len(f.Operators) > 0 {
		return f.Operators
	}
	return defaultOperatorsFor(f.Type)
}

// AllowsOperator reports whether the field permits the operator.
func (f Field) AllowsOperator(op ast.Operator) bool {
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

// HasEnumValue reports whether s is in the field's enum domain.
//
// A package-level function rather than a method on Field, deliberately: Field is
// re-exported verbatim as queryforge.Field, and a new exported method there would
// silently widen the library's public API. Callers are the validator and the
// Elasticsearch routing checks in this package.
func HasEnumValue(f Field, s string) bool {
	for _, v := range f.Values {
		if v == s {
			return true
		}
	}
	return false
}

// boolOr returns the pointed-to bool, or def when the pointer is nil (unset).
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// IsTextSearchOperator reports whether op is a string-matching operator. On a
// string/enum field these require EffectiveSearchable; on an array field the
// same tokens (e.g. contains) mean membership and are governed by filterable
// instead, so callers must scope this check by field type.
func IsTextSearchOperator(op ast.Operator) bool {
	switch op {
	case ast.OpContains, ast.OpStartsWith, ast.OpEndsWith, ast.OpRegex:
		return true
	}
	return false
}

func defaultOperatorsFor(t FieldType) []ast.Operator {
	switch t {
	case FieldString:
		return []ast.Operator{ast.OpEquals, ast.OpNotEquals, ast.OpContains, ast.OpStartsWith, ast.OpEndsWith, ast.OpIn, ast.OpNotIn, ast.OpIsNull, ast.OpIsNotNull}
	case FieldNumber:
		return []ast.Operator{ast.OpEquals, ast.OpNotEquals, ast.OpGt, ast.OpLt, ast.OpGte, ast.OpLte, ast.OpBetween, ast.OpIn, ast.OpNotIn, ast.OpIsNull, ast.OpIsNotNull}
	case FieldBoolean:
		return []ast.Operator{ast.OpEquals, ast.OpNotEquals, ast.OpIsNull, ast.OpIsNotNull}
	case FieldEnum:
		return []ast.Operator{ast.OpEquals, ast.OpNotEquals, ast.OpIn, ast.OpNotIn, ast.OpIsNull, ast.OpIsNotNull}
	case FieldDate:
		return []ast.Operator{ast.OpBefore, ast.OpAfter, ast.OpBetween, ast.OpEquals, ast.OpIsNull, ast.OpIsNotNull}
	case FieldArray:
		return []ast.Operator{ast.OpContains, ast.OpContainsAny, ast.OpContainsAll, ast.OpIsNull, ast.OpIsNotNull}
	default:
		return nil
	}
}

func ValidFieldType(t FieldType) bool {
	switch t {
	case FieldString, FieldNumber, FieldBoolean, FieldEnum, FieldDate, FieldArray:
		return true
	}
	return false
}

func IsKnownOperator(op ast.Operator) bool {
	for _, o := range ast.AllOperators {
		if o == op {
			return true
		}
	}
	return false
}
