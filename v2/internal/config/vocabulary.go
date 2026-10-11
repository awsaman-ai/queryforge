package config

import "sort"

// This file holds the experiment's closed vocabularies and the compatibility
// rules between them. Everything the engine may ever emit is named here, which
// is what makes the allowlists of section 16 of the brief enforceable: an
// operator or aggregation that is not in one of these sets cannot reach the
// compiler, whatever a config or a user sentence says.

// FieldType is the logical type of a field. It drives operator compatibility,
// aggregation compatibility, and value parsing.
//
// The set is the production library's minus "array": the first experiment
// declares no array semantics (containsAny/containsAll are out of scope), and a
// type the compiler cannot emit has no business being configurable.
type FieldType string

const (
	TypeString  FieldType = "string"
	TypeNumber  FieldType = "number"
	TypeBoolean FieldType = "boolean"
	TypeDate    FieldType = "date"
	// TypeEnum is a string drawn from a closed, declared set. It is worth
	// distinguishing from TypeString because the declared values are themselves
	// searchable semantic content — "cancelled" should match a status field —
	// and because a closed set is what lets value resolution refuse a value
	// instead of guessing.
	TypeEnum FieldType = "enum"
)

// Operator is a comparison the compiler may emit.
//
// These are the brief's short ids (section 7), not the production library's
// longer names (equals, notEquals, contains). See
// docs/decisions/004-phase-1-vocabulary.md for why the two differ.
type Operator string

const (
	OpEq  Operator = "eq"
	OpNeq Operator = "neq"
	OpGt  Operator = "gt"
	OpGte Operator = "gte"
	OpLt  Operator = "lt"
	OpLte Operator = "lte"
	OpIn  Operator = "in"
	OpNin Operator = "nin"
	// OpLike is a controlled substring match, not a user-supplied pattern. The
	// compiler owns the wildcards; user text becomes a parameter (section 16.3).
	OpLike Operator = "like"
)

// Aggregation is a function the compiler may emit.
type Aggregation string

const (
	AggCount Aggregation = "count"
	AggSum   Aggregation = "sum"
	AggAvg   Aggregation = "avg"
	AggMin   Aggregation = "min"
	AggMax   Aggregation = "max"
)

// Cardinality describes how many rows on each side of a relationship. It is
// captured now because it changes query shape later: a one-to-many join can
// duplicate parent rows, so Phase 6 needs it to decide about DISTINCT and
// GROUP BY rather than discovering the problem in a wrong result.
type Cardinality string

const (
	OneToOne  Cardinality = "one-to-one"
	OneToMany Cardinality = "one-to-many"
	ManyToOne Cardinality = "many-to-one"
)

// PatternIntent is the structural shape a query pattern stands for.
//
// PROVISIONAL. The brief lists QueryPattern as a Phase 1 model but never
// specifies it, and these shapes only acquire meaning in Phase 6's planner. The
// set is closed so that config cannot smuggle in an intent nothing implements;
// it is expected to be revisited once planning exists.
type PatternIntent string

const (
	IntentTopN       PatternIntent = "top_n"
	IntentCount      PatternIntent = "count"
	IntentAggregate  PatternIntent = "aggregate"
	IntentFilter     PatternIntent = "filter"
	IntentTimeWindow PatternIntent = "time_window"
)

// operatorsByType is the operator/field-type compatibility matrix. A pairing
// absent here is rejected at config load, so it can never reach the compiler.
//
// Two deliberate omissions, both cases where SQL would happily accept something
// that is a trap in natural language:
//
//   - No ordering comparison on strings. "customers after smith" is lexicographic
//     nonsense dressed as a question, and accepting gt on a string invites the
//     ranker to bind a comparison phrase to the wrong field.
//   - No LIKE on enum. An enum's values are a closed declared set, so a
//     substring search over them defeats the point: matching should resolve to
//     one of the declared values or be refused.
var operatorsByType = map[FieldType][]Operator{
	TypeString:  {OpEq, OpNeq, OpIn, OpNin, OpLike},
	TypeEnum:    {OpEq, OpNeq, OpIn, OpNin},
	TypeNumber:  {OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte, OpIn, OpNin},
	TypeBoolean: {OpEq, OpNeq},
	// Date ranges are expressed as a pair of half-open comparisons
	// (>= start AND < end) rather than a BETWEEN operator, matching how the
	// production library models periods. That keeps "last 6 months" from
	// silently including or excluding a boundary depending on the dialect.
	TypeDate: {OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte},
}

// aggregationsByType is the aggregation/field-type compatibility matrix.
//
// MIN/MAX are refused on strings for the same reason ordering comparisons are:
// "the smallest customer name" is not a question anyone means to ask. COUNT is
// allowed on every type because it counts rows, not magnitudes.
var aggregationsByType = map[FieldType][]Aggregation{
	TypeString:  {AggCount},
	TypeEnum:    {AggCount},
	TypeBoolean: {AggCount},
	TypeNumber:  {AggCount, AggSum, AggAvg, AggMin, AggMax},
	TypeDate:    {AggCount, AggMin, AggMax},
}

// ValidFieldType reports whether t is a supported field type.
func ValidFieldType(t FieldType) bool {
	_, ok := operatorsByType[t]
	return ok
}

// ValidOperator reports whether op is a supported operator.
func ValidOperator(op Operator) bool {
	switch op {
	case OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte, OpIn, OpNin, OpLike:
		return true
	}
	return false
}

// ValidAggregation reports whether agg is a supported aggregation.
func ValidAggregation(agg Aggregation) bool {
	switch agg {
	case AggCount, AggSum, AggAvg, AggMin, AggMax:
		return true
	}
	return false
}

// ValidCardinality reports whether c is a supported cardinality.
//
// "many-to-many" is deliberately absent: it needs a junction table, which the
// relationship model does not express and the Phase 3 scope does not include. A
// config declaring one is rejected with that explanation rather than silently
// treated as one-to-many, which would produce quietly duplicated rows.
func ValidCardinality(c Cardinality) bool {
	switch c {
	case OneToOne, OneToMany, ManyToOne:
		return true
	}
	return false
}

// ValidPatternIntent reports whether i is a supported query-pattern intent.
func ValidPatternIntent(i PatternIntent) bool {
	switch i {
	case IntentTopN, IntentCount, IntentAggregate, IntentFilter, IntentTimeWindow:
		return true
	}
	return false
}

// OperatorAllowed reports whether op may be applied to a field of type t.
func OperatorAllowed(t FieldType, op Operator) bool {
	for _, allowed := range operatorsByType[t] {
		if allowed == op {
			return true
		}
	}
	return false
}

// AggregationAllowed reports whether agg may be applied to a field of type t.
func AggregationAllowed(t FieldType, agg Aggregation) bool {
	for _, allowed := range aggregationsByType[t] {
		if allowed == agg {
			return true
		}
	}
	return false
}

// OperatorsFor returns the operators valid for t, sorted. The copy is
// deliberate: the matrix is package state read concurrently once a registry is
// built, and handing out the backing array would let a caller corrupt it.
func OperatorsFor(t FieldType) []Operator {
	out := append([]Operator(nil), operatorsByType[t]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AggregationsFor returns the aggregations valid for t, sorted.
func AggregationsFor(t FieldType) []Aggregation {
	out := append([]Aggregation(nil), aggregationsByType[t]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// FieldTypes returns every supported field type, sorted. Used in error messages,
// where a stable order is what keeps rejections byte-identical between runs.
func FieldTypes() []FieldType {
	out := make([]FieldType, 0, len(operatorsByType))
	for t := range operatorsByType {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Operators returns every supported operator, sorted.
func Operators() []Operator {
	return []Operator{OpEq, OpGt, OpGte, OpIn, OpLike, OpLt, OpLte, OpNeq, OpNin}
}

// Aggregations returns every supported aggregation, sorted.
func Aggregations() []Aggregation {
	return []Aggregation{AggAvg, AggCount, AggMax, AggMin, AggSum}
}

// Cardinalities returns every supported cardinality, sorted.
func Cardinalities() []Cardinality {
	return []Cardinality{ManyToOne, OneToMany, OneToOne}
}

// PatternIntents returns every supported pattern intent, sorted.
func PatternIntents() []PatternIntent {
	return []PatternIntent{IntentAggregate, IntentCount, IntentFilter, IntentTimeWindow, IntentTopN}
}
