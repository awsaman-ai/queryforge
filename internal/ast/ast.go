// Package ast defines the validated intermediate representation the model emits
// and every backend generator compiles from: Query, its filter tree of
// Conditions, and the typed Value leaves.
//
// It is the bottom of the dependency graph — it imports nothing outside the
// standard library — because every other package in the library speaks in terms
// of these types. The public façade re-exports all of them under their original
// names, so a caller writes queryforge.Query, never ast.Query.
package ast

import (
	"encoding/json"
	"strings"
)

// ASTVersion is the current Query AST schema version emitted by New.
const ASTVersion = "1.0"

// Query is the root AST node: a target entity, an optional filter tree, result
// shaping (sort/limit/offset), and an optional projection (select). Every
// operation it can express is a READ — there is no mutation node anywhere in
// this AST, which is how the library guarantees "GET only". Aggregation is
// deliberately deferred to keep the MVP AST small.
type Query struct {
	Version string     `json:"version"`          // AST schema version, e.g. "1.0"
	Entity  string     `json:"entity"`           // logical entity name; must match the config's entity
	Select  []string   `json:"select,omitempty"` // projection: logical field names to return; empty = all returnable fields
	Filter  *Condition `json:"filter,omitempty"` // root of the predicate tree; nil = no filter (match all)
	Sort    []SortSpec `json:"sort,omitempty"`   // ordering clauses, applied in order
	Limit   *int       `json:"limit,omitempty"`  // max rows to return; nil = fall back to config default
	Offset  *int       `json:"offset,omitempty"` // rows to skip (pagination); nil = 0
}

// SortSpec is one ORDER BY clause. Dir is "ASC" or "DESC".
type SortSpec struct {
	Field string `json:"field"`
	Dir   string `json:"dir"`
}

// CondType discriminates the Condition union.
type CondType string

const (
	CondLogical    CondType = "logical"
	CondComparison CondType = "comparison"
)

// LogicalOp is the connective of a logical Condition.
type LogicalOp string

const (
	OpAND LogicalOp = "AND"
	OpOR  LogicalOp = "OR"
	OpNOT LogicalOp = "NOT"
)

// Operator is the comparison operator of a comparison Condition. The full
// catalogue is fixed; which operators a given field may use is bounded per
// field in the config.
type Operator string

const (
	OpEquals      Operator = "equals"
	OpNotEquals   Operator = "notEquals"
	OpGt          Operator = "gt"
	OpLt          Operator = "lt"
	OpGte         Operator = "gte"
	OpLte         Operator = "lte"
	OpBetween     Operator = "between"
	OpIn          Operator = "in"
	OpNotIn       Operator = "notIn"
	OpContains    Operator = "contains"
	OpContainsAny Operator = "containsAny"
	OpContainsAll Operator = "containsAll"
	OpStartsWith  Operator = "startsWith"
	OpEndsWith    Operator = "endsWith"
	OpRegex       Operator = "regex"
	OpBefore      Operator = "before"
	OpAfter       Operator = "after"
	OpIsNull      Operator = "isNull"
	OpIsNotNull   Operator = "isNotNull"
)

// AllOperators is the fixed operator catalogue.
var AllOperators = []Operator{
	OpEquals, OpNotEquals, OpGt, OpLt, OpGte, OpLte, OpBetween,
	OpIn, OpNotIn, OpContains, OpContainsAny, OpContainsAll,
	OpStartsWith, OpEndsWith, OpRegex, OpBefore, OpAfter,
	OpIsNull, OpIsNotNull,
}

// Condition is either a Logical node (op + children) or a Comparison node
// (field + operator + value), discriminated by Type. The flat shape keeps
// JSON (de)serialization dependency-free.
type Condition struct {
	Type CondType `json:"type"`

	// Logical fields.
	Op       LogicalOp    `json:"op,omitempty"`
	Children []*Condition `json:"children,omitempty"`

	// Comparison fields.
	Field    string   `json:"field,omitempty"`
	Operator Operator `json:"operator,omitempty"`
	Value    *Value   `json:"value,omitempty"`

	// Scoped marks a predicate the calling application forced onto the query via
	// a Scope map, rather than one the user's question asked for. It steers
	// predicate ordering and lets Explain present the scope separately.
	//
	// It is deliberately excluded from JSON in both directions. That is what
	// makes it trustworthy: the flag can only ever be set by scope.Apply, which
	// runs after validation, so no model output and no request body can forge a
	// node claiming to be caller-supplied.
	Scoped bool `json:"-"`
}

// ValueKind is the tag of the Value union.
type ValueKind string

const (
	KindString       ValueKind = "string"
	KindNumber       ValueKind = "number"
	KindBoolean      ValueKind = "boolean"
	KindEnum         ValueKind = "enum"
	KindArray        ValueKind = "array"
	KindDate         ValueKind = "date"
	KindRelativeDate ValueKind = "relative_date"
)

// Value is a tagged union carrying a typed literal. For every kind except
// relative_date the payload lives in V; relative_date uses Unit + Amount
// (e.g. unit="day", amount=-30 means "30 days ago"). The tag lets the
// validator enforce, deterministically and before any query is built, that
// operators pair only with compatible value kinds.
type Value struct {
	Kind ValueKind

	// Payload for string/number/boolean/enum/date/array.
	// Numbers decode as float64 and arrays as []any (JSON semantics).
	V any

	// Payload for relative_date.
	Unit   string
	Amount int
}

// MarshalJSON emits the union in its documented on-the-wire shape. It avoids
// `omitempty` on the payload so a boolean false or a zero amount survives the
// round trip.
func (v Value) MarshalJSON() ([]byte, error) {
	if v.Kind == KindRelativeDate {
		return json.Marshal(struct {
			Kind   ValueKind `json:"kind"`
			Unit   string    `json:"unit"`
			Amount int       `json:"amount"`
		}{v.Kind, v.Unit, v.Amount})
	}
	return json.Marshal(struct {
		Kind ValueKind `json:"kind"`
		V    any       `json:"v"`
	}{v.Kind, v.V})
}

// UnmarshalJSON decodes the union, dispatching on the "kind" tag.
func (v *Value) UnmarshalJSON(data []byte) error {
	var raw struct {
		Kind   ValueKind       `json:"kind"`
		V      json.RawMessage `json:"v"`
		Unit   string          `json:"unit"`
		Amount int             `json:"amount"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v.Kind = raw.Kind
	v.Unit = raw.Unit
	v.Amount = raw.Amount
	v.V = nil
	if len(raw.V) > 0 && string(raw.V) != "null" {
		var payload any
		if err := json.Unmarshal(raw.V, &payload); err != nil {
			return err
		}
		v.V = payload
	}
	return nil
}

// AsString returns the payload as a string when the kind carries one.
func (v Value) AsString() (string, bool) {
	s, ok := v.V.(string)
	return s, ok
}

// AsBool returns the payload as a bool.
func (v Value) AsBool() (bool, bool) {
	b, ok := v.V.(bool)
	return b, ok
}

// AsFloat returns the payload as a float64 (the JSON number type).
func (v Value) AsFloat() (float64, bool) {
	f, ok := v.V.(float64)
	return f, ok
}

// AsSlice returns the payload as a slice when the kind is array.
func (v Value) AsSlice() ([]any, bool) {
	s, ok := v.V.([]any)
	return s, ok
}

// NewQuery returns an empty Query stamped with the current AST version.
func NewQuery(entity string) *Query {
	return &Query{Version: ASTVersion, Entity: entity}
}

// IntPtr is a small helper for the *int fields on Query. It is exported only so
// the tests in the other internal packages — which no longer share a package
// with Query — can keep building literals the same way.
func IntPtr(i int) *int { return &i }

// The operator predicates every layer above shares. They ask questions about an
// Operator alone — no config, no context — so this is the lowest place they can
// live, and the validator, the explainer, the generators and the planner all
// read the same answer.

// IsNullOperator reports whether the operator takes no value.
func IsNullOperator(op Operator) bool { return op == OpIsNull || op == OpIsNotNull }

// IsArrayValueOperator reports whether the operator's value must be a JSON array.
func IsArrayValueOperator(op Operator) bool {
	switch op {
	case OpBetween, OpIn, OpNotIn, OpContainsAny, OpContainsAll:
		return true
	}
	return false
}

// JoinOperators renders an operator slice for error messages.
func JoinOperators(ops []Operator) string {
	parts := make([]string, len(ops))
	for i, o := range ops {
		parts[i] = string(o)
	}
	return strings.Join(parts, ", ")
}
