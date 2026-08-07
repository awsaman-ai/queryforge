package queryforge

// Caller-supplied scope filters (v0.0.2).
//
// Some predicates are not the user's to choose. A multi-tenant application knows
// the caller's subscription, user, and enterprise before any question is asked,
// and every query must be confined to them no matter what the natural-language
// request says. Those values are not part of the query vocabulary — they are not
// something a user should be able to phrase, widen, or omit — so they do not
// belong in the config's field list and must never be exposed to the model.
//
// This file implements that: a map handed to Translate/GenerateFrom, normalized
// into typed predicates and AND-ed onto the root of the filter tree AFTER the
// model's AST has been validated. Two properties follow from where the injection
// happens, and both matter:
//
//   - The model never sees these fields, so it cannot reference, relax, or
//     negate them. It never even learns they exist.
//   - The injection is at the ROOT of the tree, so a filter the model produced
//     as `A OR B` becomes `scope AND (A OR B)`. A scope predicate can only ever
//     narrow the result set; there is no AST the model can emit that escapes it.
//
// Scope keys need not appear in the config (the common case: a tenant column the
// NLP layer should not know about). When a key IS registered, its declared type,
// enum domain, and numeric bounds are enforced, so a wrong-typed or misspelled
// enum fails loudly at the call instead of silently matching nothing.

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ErrScope tags every failure caused by the caller-supplied scope map, as
// opposed to the model's output or the config. Callers (and the HTTP facade)
// use errors.Is to tell "the calling code passed a bad scope" — a programming
// error, 400-shaped, never worth retrying — apart from a model or validation
// failure.
var ErrScope = errors.New("invalid scope")

// Scope carries additional filters supplied by the calling application rather
// than by the user's question: subscription, tenant, user, enterprise ids and
// the like. Every entry is AND-ed onto every query the engine compiles.
//
// Values may be:
//
//	scalar            string, bool, any int/uint/float, or time.Time  -> equals
//	list              a slice or array of those                       -> in
//
// On a field the config declares as an array, the same shapes mean membership
// instead: a scalar becomes contains, a list becomes containsAny.
//
// A nil or empty Scope injects nothing, so existing call sites keep their exact
// behaviour by passing nil.
type Scope map[string]any

// ScopeFilter is one normalized scope entry: the predicate that was actually
// AND-ed into the query. It is reported back on TranslateResult.Scope so an
// audit log can record precisely what was forced onto the query, without having
// to re-derive it from the map.
type ScopeFilter struct {
	Field    string   `json:"field"`    // logical field name (physical mapping applied at generation)
	Operator Operator `json:"operator"` // equals/in, or contains/containsAny on an array field
	Value    *Value   `json:"value"`    // typed literal, same union the AST uses
	Declared bool     `json:"declared"` // true when the config registers this field, so its rules were enforced
}

// String renders the filter for logs, e.g. `subscriptionId equals "SUB-42"`.
func (s ScopeFilter) String() string {
	return describeComparison(&Condition{
		Type: CondComparison, Field: s.Field, Operator: s.Operator, Value: s.Value,
	})
}

// normalizeScope turns the caller's map into an ordered, typed, validated list
// of predicates. It returns nil for an empty scope so the no-scope path costs
// nothing.
//
// Keys are processed in sorted order. That is not cosmetic: it makes the
// generated SQL, its bound-argument order, and the Mongo filter document
// byte-identical for the same input, which is what lets scope behaviour be
// covered by ordinary golden tests.
func normalizeScope(s Scope, c *Config) ([]ScopeFilter, error) {
	if len(s) == 0 {
		return nil, nil // nothing to inject: existing behaviour, unchanged
	}

	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	seen := make(map[string]string, len(s)) // trimmed name -> the raw key that claimed it
	out := make([]ScopeFilter, 0, len(keys))
	for _, k := range keys {
		name := strings.TrimSpace(k)
		if name == "" {
			return nil, fmt.Errorf("%w: a scope key is empty; every key must name a field", ErrScope)
		}
		// A scope key is the one input to the tenant-isolation guarantee that had
		// no validation at all. When the config does not declare it — the
		// documented, common case for a tenant column the NLP layer must not know
		// about — PhysicalName passes it through verbatim, and the SQL generator
		// writes it into the statement as an identifier. A key of
		// `tenant_id = 'X' OR 1=1 --` therefore became syntax, and commented out
		// the bound placeholder that was meant to follow it.
		//
		// The same key is a BSON filter key on the Mongo side, where a leading "$"
		// is read as an operator. validIdentPath rules out both, and is checked
		// before any predicate is built so nothing downstream has to.
		if !validIdentPath(name) {
			return nil, fmt.Errorf("%w: key %q is not a valid field name; a scope key must be one or more "+
				"dot-separated identifiers (letters, digits and underscores, not starting with a digit)", ErrScope, k)
		}
		// " userId" and "userId" would compile to the same predicate twice, and
		// which one wins would depend on map ordering. Reject rather than guess.
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: keys %q and %q both name field %q; remove one", ErrScope, prev, k, name)
		}
		seen[name] = k

		sf, err := scopeFilterFor(name, s[k], c)
		if err != nil {
			return nil, err
		}
		out = append(out, sf)
	}
	return out, nil
}

// scopeFilterFor converts one map entry into a typed predicate, applying the
// config's rules when the field is registered and inferring from the Go value
// when it is not.
func scopeFilterFor(name string, raw any, c *Config) (ScopeFilter, error) {
	f, declared := c.FieldByName(name) // nil+false for the usual tenant-column case

	// A nil value is genuinely ambiguous — it could mean "no filter on this key"
	// or "the column IS NULL" — and guessing either way silently changes which
	// rows come back. Make the caller say which they meant.
	if raw == nil {
		return ScopeFilter{}, fmt.Errorf("%w: field %q has a nil value; omit the key to skip the filter, "+
			"or pass an explicit value", ErrScope, name)
	}

	if elems, isList := scopeElems(raw); isList {
		return scopeListFilter(name, elems, f, declared, c)
	}
	return scopeScalarFilter(name, raw, f, declared, c)
}

// scopeScalarFilter builds an equals predicate (contains, on an array field).
func scopeScalarFilter(name string, raw any, f *Field, declared bool, c *Config) (ScopeFilter, error) {
	kind, payload, ok := scopeScalarValue(raw)
	if !ok {
		return ScopeFilter{}, fmt.Errorf("%w: field %q has unsupported value type %T; "+
			"use a string, number, bool, time.Time, or a slice of those", ErrScope, name, raw)
	}

	op := OpEquals
	if declared {
		// On an array field a bare scalar means "this element is present", which
		// is `contains`, not equality against the whole array.
		if f.Type == FieldArray {
			op = OpContains
		}
		kind = coerceKind(kind, scopeElemType(f))
	}

	v := &Value{Kind: kind, V: payload}
	cond := &Condition{Type: CondComparison, Field: name, Operator: op, Value: v}
	if err := checkScopeAgainstConfig(name, cond, f, declared, c); err != nil {
		return ScopeFilter{}, err
	}
	return ScopeFilter{Field: name, Operator: op, Value: v, Declared: declared}, nil
}

// scopeListFilter builds an in predicate (containsAny, on an array field).
func scopeListFilter(name string, elems []any, f *Field, declared bool, c *Config) (ScopeFilter, error) {
	// An empty list has no safe reading. As SQL it is a syntax error (`IN ()`);
	// read literally it matches nothing, silently emptying every result. Neither
	// is plausibly what the caller meant.
	if len(elems) == 0 {
		return ScopeFilter{}, fmt.Errorf("%w: field %q has an empty list; a list must hold at least one value "+
			"(omit the key to skip the filter)", ErrScope, name)
	}

	conv := make([]any, len(elems))
	for i, e := range elems {
		_, payload, ok := scopeScalarValue(e)
		if !ok {
			return ScopeFilter{}, fmt.Errorf("%w: field %q element %d has unsupported value type %T; "+
				"list elements must be strings, numbers, bools, or time.Time", ErrScope, name, i, e)
		}
		conv[i] = payload
	}

	op := OpIn
	if declared && f.Type == FieldArray {
		op = OpContainsAny // "any of these tags is present"
	}

	v := &Value{Kind: KindArray, V: conv}
	cond := &Condition{Type: CondComparison, Field: name, Operator: op, Value: v}
	if err := checkScopeAgainstConfig(name, cond, f, declared, c); err != nil {
		return ScopeFilter{}, err
	}
	return ScopeFilter{Field: name, Operator: op, Value: v, Declared: declared}, nil
}

// checkScopeAgainstConfig applies the config's *value* rules — declared type,
// enum domain, numeric min/max — to a scope predicate on a registered field.
//
// It deliberately skips the capability flags (queryable, filterable) and the
// per-field operator whitelist that the model's AST must satisfy. Those exist to
// bound what a *user's sentence* may reach; scope comes from the application's
// own code, and the primary use case is precisely a field hidden from the NLP
// surface (queryable:false) that every query must still be confined to. Value
// correctness, by contrast, is always worth enforcing: a misspelled enum or a
// string where a number belongs is a bug that would otherwise return zero rows
// with no explanation.
//
// Unregistered fields have no rules to check — the config says nothing about
// them — so they pass through with the type inferred from the Go value.
func checkScopeAgainstConfig(name string, cond *Condition, f *Field, declared bool, c *Config) error {
	if !declared {
		return nil
	}
	elemType := scopeElemType(f)

	var errs ValidationErrors
	if isArrayValueOperator(cond.Operator) {
		errs = validateArrayValue(c, "scope."+name, f, elemType, cond)
	} else {
		errs = validateScalarValue(c, "scope."+name, f, elemType, cond.Value)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrScope, errs.Error())
}

// scopeElemType returns the type a value is checked against: the element type
// for an array field, the field's own type otherwise.
func scopeElemType(f *Field) FieldType {
	if f.Type != FieldArray {
		return f.Type
	}
	if f.ItemType != "" {
		return f.ItemType
	}
	return FieldString
}

// coerceKind retags a kind inferred from a Go value with what the config
// declares. A Go string is KindString on its own, but must be KindEnum on an
// enum field (so the domain check fires) and KindDate on a date field (so the
// generators emit a real date rather than a bare string).
func coerceKind(inferred ValueKind, declared FieldType) ValueKind {
	if inferred != KindString {
		return inferred // numbers, bools and time.Time are already unambiguous
	}
	switch declared {
	case FieldEnum:
		return KindEnum
	case FieldDate:
		return KindDate
	default:
		return inferred
	}
}

// scopeScalarValue maps a Go scalar onto an AST value kind and the payload the
// rest of the library expects. Numbers normalize to float64 and times to RFC3339
// strings, matching exactly what JSON decoding of a model-produced AST yields —
// so a scope predicate and a model predicate on the same field are byte-identical
// downstream, and the generators need no scope-specific code path.
func scopeScalarValue(raw any) (ValueKind, any, bool) {
	// Session and claims structs routinely hold optional ids as pointers, so a
	// caller writing Scope{"userId": claims.UserID} would otherwise hit an
	// "unsupported type *string" for a perfectly ordinary value. Follow one level
	// of indirection; a nil pointer is left to fail as the ambiguous value it is.
	if rv := reflect.ValueOf(raw); rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "", nil, false
		}
		raw = rv.Elem().Interface()
	}

	switch x := raw.(type) {
	case string:
		return KindString, x, true
	case bool:
		return KindBoolean, x, true
	case time.Time:
		return KindDate, x.UTC().Format(time.RFC3339), true

	case int:
		return KindNumber, float64(x), true
	case int8:
		return KindNumber, float64(x), true
	case int16:
		return KindNumber, float64(x), true
	case int32:
		return KindNumber, float64(x), true
	case int64:
		return KindNumber, float64(x), true
	case uint:
		return KindNumber, float64(x), true
	case uint8:
		return KindNumber, float64(x), true
	case uint16:
		return KindNumber, float64(x), true
	case uint32:
		return KindNumber, float64(x), true
	case uint64:
		return KindNumber, float64(x), true
	case float32:
		return KindNumber, float64(x), true
	case float64:
		return KindNumber, x, true

	default:
		return "", nil, false
	}
}

// scopeElems returns the elements of a slice or array value. It accepts any
// element type ([]string, []int, []any, …) so callers are not forced to build
// []any by hand.
//
// []byte is rejected: it is a slice, but a caller passing one means "these
// bytes" (an id, a key) and would otherwise get an IN over 16 separate numbers.
func scopeElems(raw any) ([]any, bool) {
	if _, isBytes := raw.([]byte); isBytes {
		return nil, false // falls through to the scalar path, which reports the type
	}
	rv := reflect.ValueOf(raw)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	default:
		return nil, false
	}
}

// applyScope returns the AST that will actually be compiled: the scope
// predicates AND-ed onto the root of q's filter.
//
// q is never mutated. The engine hands out the caller's AST (and, with
// ScopeInAST off, keeps reporting it as TranslateResult.AST), so writing into it
// would mean a second GenerateFrom call on the same AST silently injecting the
// scope twice.
func applyScope(q *Query, filters []ScopeFilter) *Query {
	if q == nil || len(filters) == 0 {
		return q // nothing to do: the common, zero-cost path
	}

	children := make([]*Condition, 0, len(filters)+1)
	for _, sf := range filters {
		children = append(children, &Condition{
			Type:     CondComparison,
			Field:    sf.Field,
			Operator: sf.Operator,
			Value:    sf.Value,
			Scoped:   true, // marks this node as caller-forced, not user-asked
		})
	}

	// Splice the model's filter in beside the scope predicates. When it is
	// already an AND we lift its children rather than nesting a second AND —
	// AND is associative, so this is the same predicate with one less level of
	// parentheses in the emitted query.
	if q.Filter != nil {
		if q.Filter.Type == CondLogical && q.Filter.Op == OpAND && len(q.Filter.Children) > 0 {
			children = append(children, q.Filter.Children...)
		} else {
			children = append(children, q.Filter)
		}
	}

	out := *q // shallow copy: Select/Sort/Limit/Offset are read-only from here on
	if len(children) == 1 {
		out.Filter = children[0] // a lone scope predicate needs no AND wrapper
	} else {
		out.Filter = &Condition{Type: CondLogical, Op: OpAND, Children: children}
	}
	return &out
}

// splitScope separates the caller-injected predicates from the user-intent
// filter, so an explanation can present them as what they are — a fixed scope
// the question did not choose — instead of burying them among the user's own
// conditions. Returns (nil, cond) unchanged when no scope was injected.
func splitScope(cond *Condition) (scoped []*Condition, rest *Condition) {
	if cond == nil {
		return nil, nil
	}
	if cond.Scoped { // a single scope predicate and no user filter
		return []*Condition{cond}, nil
	}
	if cond.Type != CondLogical || cond.Op != OpAND {
		return nil, cond // scope is only ever spliced into a root AND
	}

	others := make([]*Condition, 0, len(cond.Children))
	for _, ch := range cond.Children {
		if ch != nil && ch.Scoped {
			scoped = append(scoped, ch)
			continue
		}
		others = append(others, ch)
	}
	if len(scoped) == 0 {
		return nil, cond // ordinary AND: leave it exactly as it was
	}

	switch len(others) {
	case 0:
		return scoped, nil // scope only
	case 1:
		return scoped, others[0] // drop the now-redundant AND wrapper
	default:
		return scoped, &Condition{Type: CondLogical, Op: OpAND, Children: others}
	}
}
