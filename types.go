package queryforge

import (
	"log/slog"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
	"github.com/awsaman-ai/queryforge/internal/explain"
	"github.com/awsaman-ai/queryforge/internal/failure"
	"github.com/awsaman-ai/queryforge/internal/gen"
	"github.com/awsaman-ai/queryforge/internal/observe"
	"github.com/awsaman-ai/queryforge/internal/planner"
	"github.com/awsaman-ai/queryforge/internal/policy"
	"github.com/awsaman-ai/queryforge/internal/provider"
	"github.com/awsaman-ai/queryforge/internal/scope"
	"github.com/awsaman-ai/queryforge/internal/validate"
)

// The public façade.
//
// The implementation lives in internal/ packages, one per layer, so that the
// dependency graph is a graph the compiler enforces rather than a convention
// 21,000 lines of one package could quietly break. This file is what keeps that
// invisible from the outside: every name the library has ever exported is
// re-exported here, spelled exactly as it was.
//
// Types are ALIASES (=), never definitions. An alias is the same type, so a
// *queryforge.Query and an *ast.Query are interchangeable, every method comes
// across untouched, and a type assertion or an errors.As written against the old
// package keeps matching. Constants and sentinel errors are re-exported by value,
// which for the sentinels means the identical error object — errors.Is still
// works across the seam. Constructors are thin wrappers rather than aliases so
// that `go doc` keeps showing them as functions with their original signatures.
//
// Nothing here may acquire behaviour. If a wrapper below ever needs a line of
// logic, that logic belongs in the internal package it forwards to.

// ─────────────────────────────────────────────────────────────────────────────
// AST — the validated intermediate representation (internal/ast).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// Query is the root AST node: a target entity, an optional filter tree,
	// result shaping (sort/limit/offset), and an optional projection (select).
	Query = ast.Query
	// SortSpec is one ORDER BY clause. Dir is "ASC" or "DESC".
	SortSpec = ast.SortSpec
	// Condition is either a Logical node (op + children) or a Comparison node
	// (field + operator + value), discriminated by Type.
	Condition = ast.Condition
	// Value is a tagged union carrying a typed literal.
	Value = ast.Value
	// CondType discriminates the Condition union.
	CondType = ast.CondType
	// LogicalOp is the connective of a logical Condition.
	LogicalOp = ast.LogicalOp
	// Operator is the comparison operator of a comparison Condition.
	Operator = ast.Operator
	// ValueKind is the tag of the Value union.
	ValueKind = ast.ValueKind
)

// ASTVersion is the current Query AST schema version emitted by New.
const ASTVersion = ast.ASTVersion

// Condition kinds.
const (
	CondLogical    = ast.CondLogical
	CondComparison = ast.CondComparison
)

// Logical connectives.
const (
	OpAND = ast.OpAND
	OpOR  = ast.OpOR
	OpNOT = ast.OpNOT
)

// The fixed comparison-operator catalogue.
const (
	OpEquals      = ast.OpEquals
	OpNotEquals   = ast.OpNotEquals
	OpGt          = ast.OpGt
	OpLt          = ast.OpLt
	OpGte         = ast.OpGte
	OpLte         = ast.OpLte
	OpBetween     = ast.OpBetween
	OpIn          = ast.OpIn
	OpNotIn       = ast.OpNotIn
	OpContains    = ast.OpContains
	OpContainsAny = ast.OpContainsAny
	OpContainsAll = ast.OpContainsAll
	OpStartsWith  = ast.OpStartsWith
	OpEndsWith    = ast.OpEndsWith
	OpRegex       = ast.OpRegex
	OpBefore      = ast.OpBefore
	OpAfter       = ast.OpAfter
	OpIsNull      = ast.OpIsNull
	OpIsNotNull   = ast.OpIsNotNull
)

// Value kinds.
const (
	KindString       = ast.KindString
	KindNumber       = ast.KindNumber
	KindBoolean      = ast.KindBoolean
	KindEnum         = ast.KindEnum
	KindArray        = ast.KindArray
	KindDate         = ast.KindDate
	KindRelativeDate = ast.KindRelativeDate
)

// AllOperators is the fixed operator catalogue.
var AllOperators = ast.AllOperators

// NewQuery returns an empty Query stamped with the current AST version.
func NewQuery(entity string) *Query { return ast.NewQuery(entity) }

// ─────────────────────────────────────────────────────────────────────────────
// Config — the single source of truth (internal/config).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// Config is the single source of truth: prompt context, validation
	// rulebook, field-to-backend mapping, and model selector.
	Config = config.Config
	// ModelConfig selects the AI planner target.
	ModelConfig = config.ModelConfig
	// BackendConfig maps the logical entity to a physical source per backend.
	BackendConfig = config.BackendConfig
	// Field is one registered, queryable attribute of the entity.
	Field = config.Field
	// FieldType is the logical type of a field.
	FieldType = config.FieldType
	// FieldValidators are deterministic value constraints applied during
	// validation.
	FieldValidators = config.FieldValidators
	// Defaults shape unspecified result windows.
	Defaults = config.Defaults
	// Policy carries the per-config limits and the cross-field business rules.
	Policy = config.Policy
	// FieldRequirement is one business rule.
	FieldRequirement = config.FieldRequirement
	// RequirementTrigger names the field (and optionally the operators) whose
	// presence fires a FieldRequirement.
	RequirementTrigger = config.RequirementTrigger
	// RequiredScope makes the Scope argument mandatory for this config.
	RequiredScope = config.RequiredScope
	// SourceType names how an Elasticsearch source was resolved.
	SourceType = config.SourceType
	// IndexRoutingStrategy selects how IndexRouting resolves physical indexes.
	IndexRoutingStrategy = config.IndexRoutingStrategy
	// DateGranularity is the partition size a "date" routing strategy resolves
	// to.
	DateGranularity = config.DateGranularity
	// IndexRouting configures Elasticsearch/OpenSearch business-rule index
	// routing.
	IndexRouting = config.IndexRouting
	// RoutingCondition is one equality/range test used by "rules" and "ifElse"
	// routing.
	RoutingCondition = config.RoutingCondition
	// IndexRule is one "rules"-strategy entry.
	IndexRule = config.IndexRule
	// IndexBranch is one "ifElse"-strategy entry, evaluated in order.
	IndexBranch = config.IndexBranch
	// ValueCase is a field's declared value-case rule.
	ValueCase = config.ValueCase
	// Protocol names the wire dialect a provider speaks.
	Protocol = config.Protocol
)

// The two policy.requiredScope modes: require some scope, or require named keys.
const (
	ScopeModeAny    = config.ScopeModeAny
	ScopeModeFields = config.ScopeModeFields
)

// The logical field types.
const (
	FieldString  = config.FieldString
	FieldNumber  = config.FieldNumber
	FieldBoolean = config.FieldBoolean
	FieldEnum    = config.FieldEnum
	FieldDate    = config.FieldDate
	FieldArray   = config.FieldArray
)

// How an Elasticsearch source was resolved.
const (
	SourceDirectIndex   = config.SourceDirectIndex
	SourceMultipleIndex = config.SourceMultipleIndex
	SourceAlias         = config.SourceAlias
	SourceBusinessRule  = config.SourceBusinessRule
)

// The index-routing strategies.
const (
	RoutingPattern = config.RoutingPattern
	RoutingDate    = config.RoutingDate
	RoutingRules   = config.RoutingRules
	RoutingIfElse  = config.RoutingIfElse
)

// Date-partition granularities.
const (
	GranularityYear  = config.GranularityYear
	GranularityMonth = config.GranularityMonth
	GranularityDay   = config.GranularityDay
)

// Value-case rules.
const (
	CaseAsIs  = config.CaseAsIs
	CaseLower = config.CaseLower
	CaseUpper = config.CaseUpper
)

// The wire dialects this library implements.
const (
	ProtocolOpenAI    = config.ProtocolOpenAI
	ProtocolAnthropic = config.ProtocolAnthropic
)

// LoadConfig reads and parses a JSON config file.
func LoadConfig(path string) (*Config, error) { return config.LoadConfig(path) }

// ParseConfig parses JSON config bytes, builds lookup indexes, and runs
// load-time validation.
func ParseConfig(data []byte) (*Config, error) { return config.ParseConfig(data) }

// ─────────────────────────────────────────────────────────────────────────────
// Validation — the deterministic guarantee (internal/validate).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// ErrCode is a stable, machine-readable classification of a validation
	// failure.
	ErrCode = validate.ErrCode
	// ValidationError is one finding: a code, where it was found, and prose.
	ValidationError = validate.ValidationError
	// ValidationErrors is the accumulated findings for one AST.
	ValidationErrors = validate.ValidationErrors
)

// Structure: the AST itself is malformed, independent of any config.
const (
	CodeMalformedAST   = validate.CodeMalformedAST
	CodeEntityMismatch = validate.CodeEntityMismatch
	CodeNestingTooDeep = validate.CodeNestingTooDeep
	CodeInvalidArity   = validate.CodeInvalidArity
	CodeUnknownVersion = validate.CodeUnknownVersion
	CodeFilterTooLarge = validate.CodeFilterTooLarge
	CodeListTooLong    = validate.CodeListTooLong
	CodeValueTooLong   = validate.CodeValueTooLong
	CodeDuplicateField = validate.CodeDuplicateField
	CodeInvalidRelDate = validate.CodeInvalidRelDate
)

// Regex policy.
const (
	CodeRegexNotAllowed  = validate.CodeRegexNotAllowed
	CodeRegexPatternLong = validate.CodeRegexPatternLong
	CodeRegexUnsafe      = validate.CodeRegexUnsafe
	CodeRegexDenied      = validate.CodeRegexDenied
)

// Vocabulary: the AST names something this config does not permit.
const (
	CodeUnknownField       = validate.CodeUnknownField
	CodeFieldNotQueryable  = validate.CodeFieldNotQueryable
	CodeFieldNotFilterable = validate.CodeFieldNotFilterable
	CodeFieldNotSortable   = validate.CodeFieldNotSortable
	CodeFieldNotReturnable = validate.CodeFieldNotReturnable
	CodeFieldNotSearchable = validate.CodeFieldNotSearchable
	CodeUnknownOperator    = validate.CodeUnknownOperator
	CodeOperatorNotAllowed = validate.CodeOperatorNotAllowed
)

// Values, sorting and paging.
const (
	CodeKindMismatch     = validate.CodeKindMismatch
	CodeValueOutOfDomain = validate.CodeValueOutOfDomain
	CodeValueOutOfBounds = validate.CodeValueOutOfBounds
	CodeValueRequired    = validate.CodeValueRequired
	CodeValueNotAllowed  = validate.CodeValueNotAllowed
	CodeInvalidSortDir   = validate.CodeInvalidSortDir
	CodeInvalidPaging    = validate.CodeInvalidPaging
	CodeLimitTooLarge    = validate.CodeLimitTooLarge
)

// Validate checks an AST against a config and returns nil when it is
// expressible, or a ValidationErrors listing every rule it broke. It is fully
// deterministic: no AI, no network. It is the guarantee that a hallucinated
// field, an illegal operator/type pairing, or an out-of-domain enum can never
// reach a generator.
//
// Values are judged by their payload against the type the config declares, not
// by the "kind" tag the model wrote on them. Validate reads the AST and never
// writes to it: callers may share one AST across goroutines.
func Validate(q *Query, c *Config) error { return validate.Validate(q, c) }

// ─────────────────────────────────────────────────────────────────────────────
// Policy — the cross-field business rules (internal/policy).
// ─────────────────────────────────────────────────────────────────────────────

// PolicyViolationError reports that an AST is structurally legal but breaks a
// business rule declared in Policy.Requires.
type PolicyViolationError = policy.PolicyViolationError

// ─────────────────────────────────────────────────────────────────────────────
// Scope — caller-imposed filters (internal/scope).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// Scope carries additional filters supplied by the calling application
	// rather than by the user's question.
	Scope = scope.Scope
	// ScopeFilter is one normalized scope predicate.
	ScopeFilter = scope.ScopeFilter
)

// ErrScope tags every failure caused by the caller-supplied scope map, as
// opposed to the model's output or the config.
var ErrScope = scope.ErrScope

// ─────────────────────────────────────────────────────────────────────────────
// Providers — the seam to any language model (internal/provider).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// ModelProvider is the seam between QueryForge and any language model.
	ModelProvider = provider.ModelProvider
	// OpenAIProvider calls any OpenAI-compatible /chat/completions endpoint.
	OpenAIProvider = provider.OpenAIProvider
	// AnthropicProvider calls Anthropic's native Messages API (/v1/messages).
	AnthropicProvider = provider.AnthropicProvider
	// FallbackProvider tries an ordered list of providers and returns the first
	// successful completion.
	FallbackProvider = provider.FallbackProvider
	// StubProvider is a deterministic ModelProvider for tests.
	StubProvider = provider.StubProvider
	// ProviderError is a model-call failure with its cause classified.
	ProviderError = provider.ProviderError
)

// NewOpenAIProvider builds the provider from a config model block.
func NewOpenAIProvider(m ModelConfig) *OpenAIProvider { return provider.NewOpenAIProvider(m) }

// NewAnthropicProvider builds the provider from a config model block. The key
// is read from the environment variable the config names.
func NewAnthropicProvider(m ModelConfig) *AnthropicProvider {
	return provider.NewAnthropicProvider(m)
}

// NewFallbackProvider builds a fallback chain from bare providers, labelling
// each by its position.
func NewFallbackProvider(providers ...ModelProvider) *FallbackProvider {
	return provider.NewFallbackProvider(providers...)
}

// ProviderFor selects the provider implementation for a config's model block,
// by the wire protocol the endpoint speaks.
func ProviderFor(m ModelConfig) ModelProvider { return provider.ProviderFor(m) }

// ProvidersFrom builds the provider (or the ordered fallback chain) a config
// asks for.
func ProvidersFrom(c *Config) ModelProvider { return provider.ProvidersFrom(c) }

// KnownProviders returns the provider names that have a built-in endpoint,
// sorted.
func KnownProviders() []string { return provider.KnownProviders() }

// ─────────────────────────────────────────────────────────────────────────────
// Planner — natural language to a candidate AST (internal/planner).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// Planner turns a question into a candidate Query AST via one model call.
	Planner = planner.Planner
	// RepairKind says why a previous attempt was rejected.
	RepairKind = planner.RepairKind
	// RepairHint is the feedback fed back to the model on a repair attempt.
	RepairHint = planner.RepairHint
)

// PromptVersion identifies the prompt template.
const PromptVersion = planner.PromptVersion

// Why a previous attempt was rejected.
const (
	RepairNone       = planner.RepairNone
	RepairValidation = planner.RepairValidation
	RepairParse      = planner.RepairParse
)

// NewPlanner builds a planner from a config and a model provider.
func NewPlanner(c *Config, p ModelProvider) *Planner { return planner.New(c, p) }

// ─────────────────────────────────────────────────────────────────────────────
// Observability — the optional reporting seam (internal/observe).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// Observer receives one Event per notable step of a translation.
	Observer = observe.Observer
	// Event is one observation.
	Event = observe.Event
	// EventKind names what happened.
	EventKind = observe.EventKind
	// Outcome is how a step ended.
	Outcome = observe.Outcome
)

// The three event kinds.
const (
	EventModelCall = observe.EventModelCall
	EventAttempt   = observe.EventAttempt
	EventTranslate = observe.EventTranslate
)

// How a step ended.
const (
	OutcomeOK          = observe.OutcomeOK
	OutcomeParseError  = observe.OutcomeParseError
	OutcomeValidation  = observe.OutcomeValidation
	OutcomeRefusal     = observe.OutcomeRefusal
	OutcomePolicy      = observe.OutcomePolicy
	OutcomeTransport   = observe.OutcomeTransport
	OutcomeBudgetSpent = observe.OutcomeBudgetSpent
	OutcomeCallerError = observe.OutcomeCallerError
	OutcomeGenerate    = observe.OutcomeGenerate
)

// SlogObserver returns an Observer that writes each Event to l as one
// structured record, with the field names, levels and error codes the Python
// and Java SDKs use. A nil logger returns a nil Observer, which disables
// emission rather than panicking on the first event.
func SlogObserver(l *slog.Logger) Observer { return observe.SlogObserver(l) }

// ─────────────────────────────────────────────────────────────────────────────
// Failures — the error taxonomy (internal/failure).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// FailureCode is a stable, machine-readable classification of a failed
	// operation.
	FailureCode = failure.FailureCode
	// ProviderErrorKind classifies why a model call failed, at a granularity
	// the retry loop can act on.
	ProviderErrorKind = failure.ProviderErrorKind
	// UnsupportedRequestError reports that the request cannot be expressed with
	// the vocabulary this config exposes.
	UnsupportedRequestError = failure.UnsupportedRequestError
)

// The failure-code vocabulary. Values are part of the public contract.
const (
	FailureNone           = failure.FailureNone
	FailureInvalidRequest = failure.FailureInvalidRequest
	FailureInvalidConfig  = failure.FailureInvalidConfig
	FailureUnknownBackend = failure.FailureUnknownBackend
	FailureInvalidScope   = failure.FailureInvalidScope
	FailureValidation     = failure.FailureValidation
	FailureUnsupported    = failure.FailureUnsupported
	FailurePolicy         = failure.FailurePolicy
	FailureModelOutput    = failure.FailureModelOutput
	FailureModelTransport = failure.FailureModelTransport
	FailureGenerate       = failure.FailureGenerate
	FailureTimeout        = failure.FailureTimeout
	FailureInternal       = failure.FailureInternal
)

// Why a model call failed, at retry-loop granularity.
const (
	KindAuth           = failure.KindAuth
	KindQuota          = failure.KindQuota
	KindRateLimit      = failure.KindRateLimit
	KindModelNotFound  = failure.KindModelNotFound
	KindInvalidRequest = failure.KindInvalidRequest
	KindUnavailable    = failure.KindUnavailable
	KindTimeout        = failure.KindTimeout
	KindTransport      = failure.KindTransport
	KindBadResponse    = failure.KindBadResponse
)

// Sentinel errors that classify a failed Plan call. The engine's repair loop
// keys off these: a reply we received but could not read is worth asking again
// (models occasionally emit truncated or double-brace JSON), whereas never
// reaching the model at all will not fix itself on a retry.
var (
	// ErrModelTransport means the model was not reached or refused the call —
	// network failure, auth rejection, retired model, quota block. Not
	// repairable.
	ErrModelTransport = failure.ErrModelTransport

	// ErrModelOutput means the model answered but the reply could not be parsed
	// into an AST. Repairable: resend with a hint describing what was wrong.
	ErrModelOutput = failure.ErrModelOutput
)

// Classify maps an error returned by this library onto its FailureCode. A nil
// error returns FailureNone; an error this library did not produce returns
// FailureInternal rather than a guess.
func Classify(err error) FailureCode { return failure.Classify(err) }

// ─────────────────────────────────────────────────────────────────────────────
// Generation — the backends (internal/gen).
// ─────────────────────────────────────────────────────────────────────────────

type (
	// Generator compiles a validated AST into one backend's query.
	Generator = gen.Generator
	// GenOptions carries per-call generation inputs that are not part of the
	// AST.
	GenOptions = gen.GenOptions
	// Result is a compiled backend query plus any advisories.
	Result = gen.Result
	// Registry maps a backend id to the generator that serves it.
	Registry = gen.Registry
	// SQLGenerator compiles an AST into parameterized SQL (Postgres dialect).
	SQLGenerator = gen.SQLGenerator
	// MySQLGenerator compiles an AST into parameterized MySQL.
	MySQLGenerator = gen.MySQLGenerator
	// MongoGenerator compiles an AST into a MongoDB find command.
	MongoGenerator = gen.MongoGenerator
	// MongoQuery is the compiled MongoDB query document.
	MongoQuery = gen.MongoQuery
	// MongoSortKey is one ordering clause of a MongoQuery.
	MongoSortKey = gen.MongoSortKey
	// ESGenerator compiles an AST into an Elasticsearch search request.
	ESGenerator = gen.ESGenerator
	// OpenSearchGenerator compiles an AST into an OpenSearch search request.
	OpenSearchGenerator = gen.OpenSearchGenerator
	// ESQuery is the compiled Elasticsearch/OpenSearch request.
	ESQuery = gen.ESQuery
)

// NewRegistry returns an empty generator registry.
func NewRegistry() *Registry { return gen.NewRegistry() }

// DefaultRegistry returns a registry with every built-in generator registered.
func DefaultRegistry() *Registry { return gen.DefaultRegistry() }

// ─────────────────────────────────────────────────────────────────────────────
// Explain — the deterministic prose readback (internal/explain).
// ─────────────────────────────────────────────────────────────────────────────

// Explain renders a validated AST as human-readable prose. It is fully
// deterministic — a plain rendering of the tree, with no model call and no
// execution — so it is safe to show a user before they run anything (the
// "dry run" / explain-before-execute safeguard). It never resolves relative
// dates against a clock; it describes them symbolically ("30 days ago") so the
// output is stable.
func Explain(q *Query, c *Config) string { return explain.Explain(q, c) }
