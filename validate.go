package queryforge

import (
	"fmt"
	"sort"
	"strings"
)

// ErrCode is a stable, machine-readable classification of a validation failure.
//
// It exists because Message alone is for humans: a caller that wants to react to
// a class of failure — surface config advice, count kinds of model mistake,
// branch in a UI — would otherwise have to match English prose, which silently
// breaks the moment a message is reworded. The codes are part of the API
// contract; renaming one is a breaking change, adding one is not.
type ErrCode string

const (
	// Structure: the AST itself is malformed, independent of any config.
	CodeMalformedAST   ErrCode = "malformed_ast"    // nil node, unknown condition type
	CodeEntityMismatch ErrCode = "entity_mismatch"  // AST entity is not this config's entity
	CodeNestingTooDeep ErrCode = "nesting_too_deep" // filter tree exceeds policy.maxNestingDepth
	CodeInvalidArity   ErrCode = "invalid_arity"    // wrong number of children or values

	// Field resolution and capability gates.
	CodeUnknownField       ErrCode = "unknown_field"        // no such field in the config
	CodeFieldNotQueryable  ErrCode = "field_not_queryable"  // queryable:false
	CodeFieldNotFilterable ErrCode = "field_not_filterable" // filterable:false
	CodeFieldNotSortable   ErrCode = "field_not_sortable"   // sortable:false
	CodeFieldNotReturnable ErrCode = "field_not_returnable" // returnable:false
	CodeFieldNotSearchable ErrCode = "field_not_searchable" // searchable:false

	// Operator legality.
	CodeUnknownOperator    ErrCode = "unknown_operator"     // not an operator this library knows
	CodeOperatorNotAllowed ErrCode = "operator_not_allowed" // known, but not in the field's whitelist
	CodeRegexDenied        ErrCode = "regex_denied"         // policy.denyRegexOn names this field

	// Values.
	CodeKindMismatch     ErrCode = "kind_mismatch"       // value kind does not match the field's type
	CodeValueOutOfDomain ErrCode = "value_out_of_domain" // not one of an enum field's values
	CodeValueOutOfBounds ErrCode = "value_out_of_bounds" // outside validators.min/max
	CodeValueRequired    ErrCode = "value_required"      // operator needs a value, none given
	CodeValueNotAllowed  ErrCode = "value_not_allowed"   // operator takes no value, one was given

	// Paging and ordering.
	CodeInvalidSortDir ErrCode = "invalid_sort_direction" // dir is neither ASC nor DESC
	CodeInvalidPaging  ErrCode = "invalid_paging"         // negative limit or offset
	CodeLimitTooLarge  ErrCode = "limit_too_large"        // limit above defaults.maxLimit
)

// ValidationError is one actionable problem found in an AST. Path locates the
// offending node (e.g. "filter.children[2]"), Message states the problem, and
// Suggestions offers nearest-match field names so a bounded repair loop can
// re-ask the model with a concrete hint.
//
// The json tags are deliberate: this type travels to API callers verbatim (see
// TranslateResult.Repairs), and a service should not have to define a parallel
// wire struct just to rename four fields.
type ValidationError struct {
	Code        ErrCode  `json:"code"`                  // stable classification; branch on this, not on Message
	Path        string   `json:"path"`                  // dotted/bracketed location of the node in the AST
	Field       string   `json:"field,omitempty"`       // the field involved, when relevant
	Message     string   `json:"message"`               // human-readable, repair-friendly description
	Suggestions []string `json:"suggestions,omitempty"` // nearest-match field names for "unknown field" errors
}

// Error renders the problem, appending "did you mean …" when suggestions exist.
func (e *ValidationError) Error() string {
	msg := e.Path + ": " + e.Message // prefix every message with its location
	if len(e.Suggestions) > 0 {      // only unknown-field errors carry suggestions
		msg += " (did you mean: " + strings.Join(e.Suggestions, ", ") + "?)"
	}
	return msg
}

// ValidationErrors aggregates every problem in one AST so the caller (or the
// repair loop) sees them all at once rather than one-at-a-time.
type ValidationErrors []*ValidationError

// Error joins the individual messages with "; ".
func (es ValidationErrors) Error() string {
	parts := make([]string, len(es)) // one string per underlying error
	for i, e := range es {           // render each in order
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Validate checks an AST against a config and returns nil when it is fully
// legal, or a ValidationErrors listing every problem. This stage is
// deterministic: no AI, no network. It is the guarantee that a hallucinated
// field, an illegal operator/type pairing, or an out-of-domain enum can never
// reach a generator.
//
// Values are judged by their payload against the type the config declares, not
// by the "kind" tag the model wrote on them (see valuetype.go). Validate reads
// the AST and never writes to it: callers may share one AST across goroutines.
func Validate(q *Query, c *Config) error {
	var errs ValidationErrors // accumulate; empty means valid

	if q == nil { // defensive: a nil AST is never valid
		return ValidationErrors{{Code: CodeMalformedAST, Path: "$", Message: "query is nil"}}
	}

	// The entity in the AST must match the entity this config describes.
	if q.Entity != c.Entity {
		errs = append(errs, &ValidationError{
			Code:    CodeEntityMismatch,
			Path:    "entity",
			Message: fmt.Sprintf("entity %q does not match config entity %q", q.Entity, c.Entity),
		})
	}

	// Validate the filter tree (predicates), if present.
	if q.Filter != nil {
		errs = append(errs, validateCondition(c, "filter", q.Filter)...)

		// Enforce the configured maximum nesting depth (0 = unlimited).
		if max := c.Policy.MaxNestingDepth; max > 0 {
			if d := conditionDepth(q.Filter); d > max {
				errs = append(errs, &ValidationError{
					Code:    CodeNestingTooDeep,
					Path:    "filter",
					Message: fmt.Sprintf("nesting depth %d exceeds policy limit %d", d, max),
				})
			}
		}
	}

	// Validate the projection: every selected field must exist, be queryable,
	// and be returnable.
	for i, name := range q.Select {
		path := fmt.Sprintf("select[%d]", i)
		f, ok := c.FieldByName(name)
		if !ok {
			errs = append(errs, &ValidationError{Code: CodeUnknownField, Path: path, Field: name,
				Message: fmt.Sprintf("unknown field %q", name), Suggestions: c.suggestFields(name)})
			continue
		}
		if !f.EffectiveQueryable() {
			errs = append(errs, &ValidationError{Code: CodeFieldNotQueryable, Path: path, Field: name,
				Message: fmt.Sprintf("field %q is excluded from queries", name)})
		}
		if !f.EffectiveReturnable() {
			errs = append(errs, &ValidationError{Code: CodeFieldNotReturnable, Path: path, Field: name,
				Message: fmt.Sprintf("field %q is not returnable", name)})
		}
	}

	// Validate sort clauses: field must exist, be queryable and sortable; dir
	// must be ASC/DESC (empty is allowed and defaulted at generation time).
	for i, s := range q.Sort {
		path := fmt.Sprintf("sort[%d]", i)
		f, ok := c.FieldByName(s.Field)
		if !ok {
			errs = append(errs, &ValidationError{Code: CodeUnknownField, Path: path, Field: s.Field,
				Message: fmt.Sprintf("unknown field %q", s.Field), Suggestions: c.suggestFields(s.Field)})
		} else {
			if !f.EffectiveQueryable() {
				errs = append(errs, &ValidationError{Code: CodeFieldNotQueryable, Path: path, Field: s.Field,
					Message: fmt.Sprintf("field %q is excluded from queries", s.Field)})
			}
			if !f.EffectiveSortable() {
				errs = append(errs, &ValidationError{Code: CodeFieldNotSortable, Path: path, Field: s.Field,
					Message: fmt.Sprintf("field %q is not sortable", s.Field)})
			}
		}
		switch strings.ToUpper(s.Dir) { // accept ASC/DESC in any case, or empty
		case "", "ASC", "DESC":
		default:
			errs = append(errs, &ValidationError{Code: CodeInvalidSortDir, Path: path,
				Message: fmt.Sprintf("invalid sort direction %q (use ASC or DESC)", s.Dir)})
		}
	}

	// Validate paging: negative windows are nonsensical; a limit above the
	// configured ceiling is rejected outright.
	if q.Limit != nil && *q.Limit < 0 {
		errs = append(errs, &ValidationError{Code: CodeInvalidPaging, Path: "limit",
			Message: fmt.Sprintf("limit %d must not be negative", *q.Limit)})
	}
	if q.Offset != nil && *q.Offset < 0 {
		errs = append(errs, &ValidationError{Code: CodeInvalidPaging, Path: "offset",
			Message: fmt.Sprintf("offset %d must not be negative", *q.Offset)})
	}
	if q.Limit != nil && c.Defaults.MaxLimit > 0 && *q.Limit > c.Defaults.MaxLimit {
		errs = append(errs, &ValidationError{Code: CodeLimitTooLarge, Path: "limit",
			Message: fmt.Sprintf("limit %d exceeds maxLimit %d", *q.Limit, c.Defaults.MaxLimit)})
	}

	if len(errs) == 0 { // return a true nil error, not a typed nil slice
		return nil
	}
	return errs
}

// validateCondition recursively validates one node of the filter tree.
func validateCondition(c *Config, path string, cond *Condition) ValidationErrors {
	if cond == nil { // a nil child is a structural error
		return ValidationErrors{{Code: CodeMalformedAST, Path: path, Message: "condition is nil"}}
	}
	switch cond.Type {
	case CondLogical:
		return validateLogical(c, path, cond)
	case CondComparison:
		return validateComparison(c, path, cond)
	default:
		return ValidationErrors{{Code: CodeMalformedAST, Path: path,
			Message: fmt.Sprintf("unknown condition type %q (expected logical or comparison)", cond.Type)}}
	}
}

// validateLogical checks a logical (AND/OR/NOT) node and recurses.
func validateLogical(c *Config, path string, cond *Condition) ValidationErrors {
	var errs ValidationErrors

	switch cond.Op { // the connective must be one of the three known ones
	case OpAND, OpOR, OpNOT:
	default:
		errs = append(errs, &ValidationError{Code: CodeUnknownOperator, Path: path,
			Message: fmt.Sprintf("unknown logical operator %q", cond.Op)})
	}

	if len(cond.Children) == 0 { // an empty logical node has no meaning
		errs = append(errs, &ValidationError{Code: CodeInvalidArity, Path: path,
			Message: fmt.Sprintf("logical %q has no children", cond.Op)})
	}
	if cond.Op == OpNOT && len(cond.Children) != 1 { // NOT negates exactly one subtree
		errs = append(errs, &ValidationError{Code: CodeInvalidArity, Path: path,
			Message: fmt.Sprintf("NOT must have exactly one child, got %d", len(cond.Children))})
	}

	for i, child := range cond.Children { // recurse into each child
		childPath := fmt.Sprintf("%s.children[%d]", path, i)
		errs = append(errs, validateCondition(c, childPath, child)...)
	}
	return errs
}

// validateComparison checks a single comparison predicate: field existence and
// capability, operator legality, and value shape/domain.
func validateComparison(c *Config, path string, cond *Condition) ValidationErrors {
	var errs ValidationErrors

	// 1) The field must be registered. Unknown -> reject with suggestions and
	//    stop (nothing else can be checked meaningfully).
	f, ok := c.FieldByName(cond.Field)
	if !ok {
		return ValidationErrors{{Code: CodeUnknownField, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("unknown field %q", cond.Field), Suggestions: c.suggestFields(cond.Field)}}
	}

	// 2) Capability gates: the field must be exposed and usable in a filter.
	if !f.EffectiveQueryable() {
		return ValidationErrors{{Code: CodeFieldNotQueryable, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("field %q is excluded from queries", cond.Field)}}
	}
	if !f.EffectiveFilterable() {
		return ValidationErrors{{Code: CodeFieldNotFilterable, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("field %q is not filterable", cond.Field)}}
	}

	// 3) Operator must be known, permitted for this field, and satisfy the
	//    text-search / regex policies.
	if !isKnownOperator(cond.Operator) {
		return ValidationErrors{{Code: CodeUnknownOperator, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("unknown operator %q", cond.Operator)}}
	}
	if !f.AllowsOperator(cond.Operator) {
		return ValidationErrors{{Code: CodeOperatorNotAllowed, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q not allowed on field %q (allowed: %s)",
				cond.Operator, cond.Field, joinOperators(f.EffectiveOperators()))}}
	}
	// Text-search operators on a string/enum field require the searchable flag.
	if (f.Type == FieldString || f.Type == FieldEnum) && isTextSearchOperator(cond.Operator) && !f.EffectiveSearchable() {
		errs = append(errs, &ValidationError{Code: CodeFieldNotSearchable, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("field %q is not searchable, so %q is not allowed", cond.Field, cond.Operator)})
	}
	// The denyRegexOn policy blocks regex on named fields (ReDoS / PII safety).
	if cond.Operator == OpRegex && c.regexDenied(cond.Field) {
		errs = append(errs, &ValidationError{Code: CodeRegexDenied, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("regex is denied on field %q by policy", cond.Field)})
	}

	// 4) Value shape. Null operators take no value; everything else requires one.
	if isNullOperator(cond.Operator) {
		if cond.Value != nil {
			errs = append(errs, &ValidationError{Code: CodeValueNotAllowed, Path: path, Field: cond.Field,
				Message: fmt.Sprintf("%q takes no value", cond.Operator)})
		}
		return errs // no further value checks apply to null operators
	}
	if cond.Value == nil {
		errs = append(errs, &ValidationError{Code: CodeValueRequired, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q requires a value", cond.Operator)})
		return errs
	}

	// The scalar/element type to check values against. For an array field the
	// relevant type is its element type (itemType), which is what `contains`
	// and `containsAll/Any` compare against.
	elemType := scalarTypeOf(f)

	if isArrayValueOperator(cond.Operator) {
		errs = append(errs, validateArrayValue(c, path, f, elemType, cond)...)
	} else {
		errs = append(errs, validateScalarValue(c, path, f, elemType, cond.Value)...)
	}
	return errs
}

// validateArrayValue checks operators whose value must be a JSON array
// (between/in/notIn/containsAny/containsAll), including per-element typing.
func validateArrayValue(c *Config, path string, f *Field, elemType FieldType, cond *Condition) ValidationErrors {
	var errs ValidationErrors

	// The payload must actually be a list. Asking the tag instead used to fail
	// a perfectly good `in` that the model had tagged "enum" — and, worse, to
	// pass a tag of "array" over a lone scalar, which then bound as nothing.
	elems, ok := cond.Value.AsSlice()
	if !ok {
		return ValidationErrors{{Code: CodeKindMismatch, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q expects an array value, got %s", cond.Operator, describePayload(cond.Value))}}
	}

	if cond.Operator == OpBetween && len(elems) != 2 { // BETWEEN is a 2-tuple
		errs = append(errs, &ValidationError{Code: CodeInvalidArity, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("between requires exactly 2 values, got %d", len(elems))})
	}
	if cond.Operator != OpBetween && len(elems) == 0 { // in/contains* need at least one
		errs = append(errs, &ValidationError{Code: CodeInvalidArity, Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q requires at least one value", cond.Operator)})
	}

	for i, e := range elems { // type-check each element against the element type
		if !elementKindOK(elemType, e) {
			errs = append(errs, &ValidationError{Code: CodeKindMismatch, Path: fmt.Sprintf("%s.value[%d]", path, i), Field: cond.Field,
				Message: fmt.Sprintf("element %d is not a valid %s", i, elemType)})
			continue
		}
		if elemType == FieldEnum { // enum elements must be in-domain
			if s, ok := e.(string); ok && !f.hasEnumValue(s) {
				errs = append(errs, &ValidationError{Code: CodeValueOutOfDomain, Path: fmt.Sprintf("%s.value[%d]", path, i), Field: cond.Field,
					Message: fmt.Sprintf("%q is not a valid value for %q (allowed: %s)", s, cond.Field, strings.Join(f.Values, ", "))})
			}
		}
		if elemType == FieldNumber { // numeric elements honor min/max validators
			if n, ok := e.(float64); ok {
				errs = append(errs, f.checkNumericBounds(path, n)...)
			}
		}
	}
	return errs
}

// validateScalarValue checks operators whose value is a single scalar.
func validateScalarValue(c *Config, path string, f *Field, elemType FieldType, v *Value) ValidationErrors {
	var errs ValidationErrors

	if !scalarKindOK(elemType, v) { // the value itself must fit the (element) type
		// Name what actually arrived rather than the kind tag: after
		// normalization the tag can be the declared one while the payload is
		// not, and "kind number is not compatible with type number" would be a
		// riddle both for a human and for the model reading the repair hint.
		return ValidationErrors{{Code: CodeKindMismatch, Path: path, Field: f.Name,
			Message: fmt.Sprintf("value %s is not compatible with field type %q", describePayload(v), elemType)}}
	}

	if elemType == FieldEnum { // enum scalar must be in-domain
		if s, ok := v.AsString(); ok && !f.hasEnumValue(s) {
			errs = append(errs, &ValidationError{Code: CodeValueOutOfDomain, Path: path, Field: f.Name,
				Message: fmt.Sprintf("%q is not a valid value for %q (allowed: %s)", s, f.Name, strings.Join(f.Values, ", "))})
		}
	}
	if elemType == FieldNumber { // numeric scalar honors min/max validators
		if n, ok := v.AsFloat(); ok {
			errs = append(errs, f.checkNumericBounds(path, n)...)
		}
	}
	return errs
}

// --- small deterministic helpers ---

// scalarKindOK reports whether a scalar Value can stand for the given type.
//
// The check is on the PAYLOAD, not on the value's kind tag. The tag is the
// model's claim about the literal; the payload is the literal. Trusting the tag
// failed in both directions: it rejected {"kind":"enum","v":"DELIVERED"} on a
// string field, where the literal was perfectly good, and it accepted
// {"kind":"number","v":"42"} on a number field, where the literal was text that
// AsFloat later dropped — binding 0 and silently returning the wrong rows.
// Deciding on the payload cannot do either. normalizeValueKinds has already
// brought the tag into line wherever the payload allows it (kindnorm.go).
func scalarKindOK(t FieldType, v *Value) bool {
	switch t {
	case FieldString, FieldEnum: // both are carried as text; enum adds a domain check
		_, ok := v.V.(string)
		return ok
	case FieldNumber:
		_, ok := v.V.(float64) // JSON decodes every number as float64
		return ok
	case FieldBoolean:
		_, ok := v.V.(bool)
		return ok
	case FieldDate:
		if v.Kind == KindRelativeDate {
			return true // unit/amount, resolved against the clock at generation time
		}
		// An absolute date must be text the generators can actually parse.
		// Without this the payload rule would admit "last tuesday" and bind the
		// prose as if it were a timestamp.
		s, ok := v.V.(string)
		return ok && isDateString(s)
	default:
		return false // arrays are never a scalar value
	}
}

// describePayload names what a value actually carries, in the vocabulary the
// config uses for types, so a mismatch message compares like with like.
func describePayload(v *Value) string {
	if v.Kind == KindRelativeDate {
		return "relative date"
	}
	switch p := v.V.(type) {
	case string:
		if isDateString(p) {
			return fmt.Sprintf("date %q", p)
		}
		return fmt.Sprintf("text %q", p)
	case float64:
		return fmt.Sprintf("number %g", p)
	case bool:
		return fmt.Sprintf("boolean %t", p)
	case []any:
		return "list"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("kind %q", v.Kind) // unreachable for JSON payloads
	}
}

// elementKindOK reports whether a raw JSON array element matches a scalar type.
// JSON decodes numbers as float64, strings as string, booleans as bool.
func elementKindOK(t FieldType, e any) bool {
	switch t {
	case FieldString, FieldEnum:
		_, ok := e.(string)
		return ok
	case FieldDate:
		// Same rule as the scalar path: a date element must be parseable, or a
		// `between` over two words would compile into a range query.
		s, ok := e.(string)
		return ok && isDateString(s)
	case FieldNumber:
		_, ok := e.(float64)
		return ok
	case FieldBoolean:
		_, ok := e.(bool)
		return ok
	default:
		return false
	}
}

// isNullOperator reports whether the operator takes no value.
func isNullOperator(op Operator) bool { return op == OpIsNull || op == OpIsNotNull }

// isArrayValueOperator reports whether the operator's value must be a JSON array.
func isArrayValueOperator(op Operator) bool {
	switch op {
	case OpBetween, OpIn, OpNotIn, OpContainsAny, OpContainsAll:
		return true
	}
	return false
}

// conditionDepth returns the nesting depth of a filter subtree: a comparison is
// depth 1, a logical node is 1 + the deepest child.
func conditionDepth(cond *Condition) int {
	if cond == nil {
		return 0
	}
	if cond.Type == CondComparison {
		return 1
	}
	deepest := 0
	for _, ch := range cond.Children {
		if d := conditionDepth(ch); d > deepest {
			deepest = d
		}
	}
	return deepest + 1
}

// joinOperators renders an operator slice for error messages.
func joinOperators(ops []Operator) string {
	parts := make([]string, len(ops))
	for i, o := range ops {
		parts[i] = string(o)
	}
	return strings.Join(parts, ", ")
}

// hasEnumValue reports whether s is in the field's enum domain.
func (f Field) hasEnumValue(s string) bool {
	for _, v := range f.Values {
		if v == s {
			return true
		}
	}
	return false
}

// checkNumericBounds applies the field's optional min/max validators to n.
func (f Field) checkNumericBounds(path string, n float64) ValidationErrors {
	if f.Validators == nil {
		return nil
	}
	var errs ValidationErrors
	if f.Validators.Min != nil && n < *f.Validators.Min {
		errs = append(errs, &ValidationError{Code: CodeValueOutOfBounds, Path: path, Field: f.Name,
			Message: fmt.Sprintf("value %g is below minimum %g", n, *f.Validators.Min)})
	}
	if f.Validators.Max != nil && n > *f.Validators.Max {
		errs = append(errs, &ValidationError{Code: CodeValueOutOfBounds, Path: path, Field: f.Name,
			Message: fmt.Sprintf("value %g is above maximum %g", n, *f.Validators.Max)})
	}
	return errs
}

// regexDenied reports whether policy forbids regex on the named field.
func (c *Config) regexDenied(field string) bool {
	for _, name := range c.Policy.DenyRegexOn {
		if name == field {
			return true
		}
	}
	return false
}

// suggestFields returns up to three registered, queryable field names closest
// to name by edit distance — the "did you mean" hint that powers repair.
func (c *Config) suggestFields(name string) []string {
	type cand struct {
		name string
		dist int
	}
	lname := strings.ToLower(name)
	var cands []cand
	for i := range c.Fields {
		f := &c.Fields[i]
		if !f.EffectiveQueryable() {
			continue // never suggest a hidden field
		}
		best := levenshtein(lname, strings.ToLower(f.Name)) // distance to the name
		for _, syn := range f.Synonyms {                    // ...or its closest synonym
			if d := levenshtein(lname, strings.ToLower(syn)); d < best {
				best = d
			}
		}
		cands = append(cands, cand{f.Name, best})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist }) // nearest first

	// Only keep genuinely close matches so nonsense doesn't get suggestions.
	limit := len(lname)/2 + 2
	var out []string
	for _, c := range cands {
		if c.dist <= limit && len(out) < 3 {
			out = append(out, c.name)
		}
	}
	return out
}

// levenshtein computes the edit distance between two strings using a single
// rolling row (O(len(a)*len(b)) time, O(len(b)) space).
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1) // distances for the previous row
	for j := 0; j <= lb; j++ {
		prev[j] = j // edit distance from "" to b[:j]
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i // edit distance from a[:i] to ""
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0 // matching characters cost nothing
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost) // delete/insert/substitute
		}
		prev = cur
	}
	return prev[lb]
}

// min3 returns the smallest of three ints.
func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
