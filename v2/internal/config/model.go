// Package config is the typed configuration model for the v2 semantic engine,
// plus its loader, validator and schema registry.
//
// Every semantic decision the engine makes must be grounded in this config
// rather than in Go code (brief, section 9). That is not only a tidiness rule:
// the research question is whether CONFIG-DRIVEN semantics can replace an LLM,
// so anything hardcoded in Go is a thumb on the scale that would make the
// Phase 10 result meaningless.
//
// The model is also the security boundary. Section 16.2 requires that every
// identifier the compiler emits originates in validated config; the types here
// are where "validated" is defined.
package config

// Config is a loaded, structurally decoded configuration. It is not necessarily
// valid: Load runs Validate, but Decode alone does not, so that tests can build
// deliberately broken configs without writing them to disk.
type Config struct {
	Entities []Entity `json:"entities"`
	Fields   []Field  `json:"fields"`

	// OperatorPhrases maps an operator to the natural-language phrases that
	// signal it: "gt" ← "more than", "over", "exceeds". These phrases become
	// corpus entries in Phase 2 and are the only way a user sentence can reach
	// an operator — there is no hardcoded phrase list in Go.
	OperatorPhrases map[Operator][]string `json:"operators"`

	// AggregationPhrases does the same for aggregations: "sum" ← "total",
	// "sum of". Optional; an aggregation with no phrases simply cannot be
	// reached from natural language, which is a legitimate way to disable one.
	AggregationPhrases map[Aggregation][]string `json:"aggregations"`

	Relationships []Relationship `json:"relationships"`
	QueryPatterns []QueryPattern `json:"queryPatterns"`

	// Security holds the complexity limits from section 16.7. It is a value, not
	// a pointer, so an absent block yields zero values that Normalize fills with
	// defaults — a config that omits it is still protected.
	Security SecurityPolicy `json:"security"`

	// Ranking optionally sets this schema's ranking thresholds (Phase 7). The
	// calibrated defaults (decision 011) apply to any value left unset. It lives
	// in config, not in SecurityPolicy: loosening a threshold is calibration,
	// not a security decision. Command-line flags still override it, and the
	// CLI announces the override.
	Ranking RankingThresholds `json:"ranking"`
}

// RankingThresholds are optional per-schema ranking thresholds. Pointers so
// that "unset" (use the calibrated default) differs from an explicit zero.
type RankingThresholds struct {
	MinScore  *float64 `json:"minScore"`
	MinMargin *float64 `json:"minMargin"`
}

// Entity is a queryable thing, mapped to exactly one table in the SQL-only first
// experiment.
type Entity struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Synonyms are alternative ways users name this entity ("client", "buyer").
	// They are corpus content, not aliases resolved by string equality.
	Synonyms []string `json:"synonyms"`
	// Table is the SQL identifier. It defaults to Name when omitted and is then
	// validated as an identifier, so an entity whose natural name is not a legal
	// identifier ("customer account") must map its table explicitly.
	Table string `json:"table"`
}

// Field is one attribute of an entity.
type Field struct {
	Entity      string    `json:"entity"`
	Name        string    `json:"name"`
	Type        FieldType `json:"type"`
	Description string    `json:"description"`
	Synonyms    []string  `json:"synonyms"`

	// Column is the SQL identifier, defaulting to Name. Same reasoning as
	// Entity.Table: the natural name and the column name are allowed to differ,
	// and only the column is ever emitted.
	Column string `json:"column"`

	// ValueAliases maps a natural-language value to the stored value:
	// "India" → "IN". Restricted to string and enum fields; see the validator.
	ValueAliases map[string]string `json:"valueAliases"`

	// EnumValues is the closed set of stored values for a TypeEnum field, and
	// must be empty for every other type. Being closed is what lets value
	// resolution refuse an unknown value rather than pass it through.
	EnumValues []string `json:"enumValues"`

	// Operators narrows the operators allowed on this field to a subset of what
	// its type permits. Empty means "everything the type allows"; it can never
	// widen beyond the type matrix.
	Operators []Operator `json:"operators"`

	// Aggregations narrows aggregations the same way.
	Aggregations []Aggregation `json:"aggregations"`

	// Filterable and Selectable default to true via the *bool nil check in
	// Normalize. They exist because "present in the schema" and "allowed in a
	// query" are different questions — an internal column may need to be
	// joinable without being exposed to natural language.
	Filterable *bool `json:"filterable"`
	Selectable *bool `json:"selectable"`
}

// Endpoint is one side of a relationship: which field of which entity carries
// the join key.
type Endpoint struct {
	Entity string `json:"entity"`
	Field  string `json:"field"`
}

// Relationship is a configured, traversable edge between two entities. Section
// 16.6 makes this an allowlist: a join path that is not configured here cannot
// be produced, however plausible it looks to the ranker.
type Relationship struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Synonyms are how a user might refer to the traversal itself — "bought",
	// "ordered", "placed". They matter because a relationship is often the only
	// thing a verb in the sentence corresponds to.
	Synonyms    []string    `json:"synonyms"`
	From        Endpoint    `json:"from"`
	To          Endpoint    `json:"to"`
	Cardinality Cardinality `json:"cardinality"`
}

// QueryPattern is a deterministic structural template for a common intent:
// "top N customers" → ORDER BY <metric> DESC LIMIT N.
//
// PROVISIONAL, see PatternIntent. Phase 2 consumes only Name, Description and
// Phrases; Intent is validated but nothing acts on it until Phase 6.
type QueryPattern struct {
	Name        string        `json:"name"`
	Intent      PatternIntent `json:"intent"`
	Description string        `json:"description"`
	// Phrases are the natural-language forms of this pattern. Unlike an entity's
	// synonyms these are structural, not vocabulary: "top N", "how many".
	Phrases []string `json:"phrases"`
	// Entity optionally restricts the pattern to one entity. Empty means it
	// applies anywhere.
	Entity string `json:"entity"`
	// Order is the direction a top_n pattern ranks in: "desc" for "top N" and
	// "highest", "asc" for "bottom N" and "lowest". Added in Phase 6, when the
	// planner first needed it (decision 015). Optional at load so that configs
	// written before Phase 6 still load; a top_n pattern without it is refused
	// when a sentence invokes it, because guessing a direction would silently
	// return the opposite rows. Only top_n patterns may declare it.
	Order SortOrder `json:"order"`
}

// SortOrder is the direction of an ORDER BY.
type SortOrder string

const (
	OrderAsc  SortOrder = "asc"
	OrderDesc SortOrder = "desc"
)

// SecurityPolicy holds the complexity limits from section 16.7.
//
// These are refusal thresholds, not tuning knobs: exceeding one is an
// INVALID_INPUT or SECURITY_REJECTED failure, never a truncation. Silently
// trimming an over-long input would answer a different question than the one
// asked, which is worse than refusing.
//
// Confidence thresholds live elsewhere on purpose. A minimum score and a
// top1/top2 margin (section 4.15) are ranking calibration that Phase 10 tunes
// against the evaluation set; mixing them in here would imply that loosening a
// threshold is a security decision.
//
// What each limit counts (Phase 8, decision 021), and where it is enforced:
//
//	maxInputLength     characters (runes) of the request          pipeline, before anything runs
//	maxPhrases         candidate spans extraction produced        pipeline, before retrieval
//	maxCandidates      candidates retrieved for ONE span          pipeline.New (declared depth)
//	                                                              and per request (actual count)
//	maxJoins           tables joined to the root, anywhere in     graph search (per path) and
//	                   the statement, subqueries included         compiler (whole query)
//	maxFilters         conditions (WHERE + HAVING)                binding and compiler
//	maxNestingDepth    subquery levels (EXISTS / COUNT)           compiler
//	maxQueryLength     bytes of the compiled SQL                  compiler
//	maxPlanningMillis  wall time of the deterministic stages      pipeline, at each stage
//	                   (rank, bind, plan, compile)                boundary
type SecurityPolicy struct {
	MaxInputLength    int `json:"maxInputLength"`
	MaxPhrases        int `json:"maxPhrases"`
	MaxCandidates     int `json:"maxCandidates"`
	MaxJoins          int `json:"maxJoins"`
	MaxFilters        int `json:"maxFilters"`
	MaxNestingDepth   int `json:"maxNestingDepth"`
	MaxQueryLength    int `json:"maxQueryLength"`
	MaxPlanningMillis int `json:"maxPlanningMillis"`
}

// DefaultSecurityPolicy is applied where a config leaves a limit at zero.
//
// The numbers are starting points chosen to be generous enough not to reject
// any query in the declared scope, and small enough to bound work. They are
// calibrated in Phase 9 against measurements, not defended as optimal now.
//
// maxPhrases was 64 until Phase 8, when it was first enforced. It counts
// candidate spans, about four per word (decision 010's overlapping n-grams),
// so 64 would refuse ordinary sentences over ~17 words. 256 is the limit
// Phase 3 already applied (nlp.DefaultOptions.MaxSpans), so enforcing the
// config value changed no earlier result (decision 021). The user approved 256
// as the default at the Phase 8 gate (decision 022 §4).
//
// maxNestingDepth 1 is what the grammar produces: a to-many condition or
// count is one correlated subquery, never a subquery inside one.
var DefaultSecurityPolicy = SecurityPolicy{
	MaxInputLength:    512,
	MaxPhrases:        256,
	MaxCandidates:     256,
	MaxJoins:          4,
	MaxFilters:        16,
	MaxNestingDepth:   1,
	MaxQueryLength:    8192,
	MaxPlanningMillis: 2000,
}

// IsFilterable reports whether the field may appear in a WHERE clause.
func (f *Field) IsFilterable() bool { return f.Filterable == nil || *f.Filterable }

// IsSelectable reports whether the field may appear in a SELECT list.
func (f *Field) IsSelectable() bool { return f.Selectable == nil || *f.Selectable }

// AllowedOperators returns the operators this field actually permits: its
// declared subset if it has one, otherwise everything its type allows.
//
// The result is always within the type matrix. Validation rejects a declared
// operator outside it, so this cannot widen the allowlist.
func (f *Field) AllowedOperators() []Operator {
	if len(f.Operators) == 0 {
		return OperatorsFor(f.Type)
	}
	return append([]Operator(nil), f.Operators...)
}

// AllowedAggregations returns the aggregations this field permits, with the same
// narrowing rule as AllowedOperators.
func (f *Field) AllowedAggregations() []Aggregation {
	if len(f.Aggregations) == 0 {
		return AggregationsFor(f.Type)
	}
	return append([]Aggregation(nil), f.Aggregations...)
}
