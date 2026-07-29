package queryforge

import (
	"fmt"
	"sort"
	"strings"
)

// ValidationError is one actionable problem found in an AST. Path locates the
// offending node (e.g. "filter.children[2]"), Message states the problem, and
// Suggestions offers nearest-match field names so a bounded repair loop can
// re-ask the model with a concrete hint.
type ValidationError struct {
	Path        string   // dotted/bracketed location of the node in the AST
	Field       string   // the field involved, when relevant
	Message     string   // human-readable, repair-friendly description
	Suggestions []string // nearest-match field names for "unknown field" errors
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
func Validate(q *Query, c *Config) error {
	var errs ValidationErrors // accumulate; empty means valid

	if q == nil { // defensive: a nil AST is never valid
		return ValidationErrors{{Path: "$", Message: "query is nil"}}
	}

	// The entity in the AST must match the entity this config describes.
	if q.Entity != c.Entity {
		errs = append(errs, &ValidationError{
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
			errs = append(errs, &ValidationError{Path: path, Field: name,
				Message: fmt.Sprintf("unknown field %q", name), Suggestions: c.suggestFields(name)})
			continue
		}
		if !f.EffectiveQueryable() {
			errs = append(errs, &ValidationError{Path: path, Field: name,
				Message: fmt.Sprintf("field %q is excluded from queries", name)})
		}
		if !f.EffectiveReturnable() {
			errs = append(errs, &ValidationError{Path: path, Field: name,
				Message: fmt.Sprintf("field %q is not returnable", name)})
		}
	}

	// Validate sort clauses: field must exist, be queryable and sortable; dir
	// must be ASC/DESC (empty is allowed and defaulted at generation time).
	for i, s := range q.Sort {
		path := fmt.Sprintf("sort[%d]", i)
		f, ok := c.FieldByName(s.Field)
		if !ok {
			errs = append(errs, &ValidationError{Path: path, Field: s.Field,
				Message: fmt.Sprintf("unknown field %q", s.Field), Suggestions: c.suggestFields(s.Field)})
		} else {
			if !f.EffectiveQueryable() {
				errs = append(errs, &ValidationError{Path: path, Field: s.Field,
					Message: fmt.Sprintf("field %q is excluded from queries", s.Field)})
			}
			if !f.EffectiveSortable() {
				errs = append(errs, &ValidationError{Path: path, Field: s.Field,
					Message: fmt.Sprintf("field %q is not sortable", s.Field)})
			}
		}
		switch strings.ToUpper(s.Dir) { // accept ASC/DESC in any case, or empty
		case "", "ASC", "DESC":
		default:
			errs = append(errs, &ValidationError{Path: path,
				Message: fmt.Sprintf("invalid sort direction %q (use ASC or DESC)", s.Dir)})
		}
	}

	// Validate paging: negative windows are nonsensical; a limit above the
	// configured ceiling is rejected outright.
	if q.Limit != nil && *q.Limit < 0 {
		errs = append(errs, &ValidationError{Path: "limit",
			Message: fmt.Sprintf("limit %d must not be negative", *q.Limit)})
	}
	if q.Offset != nil && *q.Offset < 0 {
		errs = append(errs, &ValidationError{Path: "offset",
			Message: fmt.Sprintf("offset %d must not be negative", *q.Offset)})
	}
	if q.Limit != nil && c.Defaults.MaxLimit > 0 && *q.Limit > c.Defaults.MaxLimit {
		errs = append(errs, &ValidationError{Path: "limit",
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
		return ValidationErrors{{Path: path, Message: "condition is nil"}}
	}
	switch cond.Type {
	case CondLogical:
		return validateLogical(c, path, cond)
	case CondComparison:
		return validateComparison(c, path, cond)
	default:
		return ValidationErrors{{Path: path,
			Message: fmt.Sprintf("unknown condition type %q (expected logical or comparison)", cond.Type)}}
	}
}

// validateLogical checks a logical (AND/OR/NOT) node and recurses.
func validateLogical(c *Config, path string, cond *Condition) ValidationErrors {
	var errs ValidationErrors

	switch cond.Op { // the connective must be one of the three known ones
	case OpAND, OpOR, OpNOT:
	default:
		errs = append(errs, &ValidationError{Path: path,
			Message: fmt.Sprintf("unknown logical operator %q", cond.Op)})
	}

	if len(cond.Children) == 0 { // an empty logical node has no meaning
		errs = append(errs, &ValidationError{Path: path,
			Message: fmt.Sprintf("logical %q has no children", cond.Op)})
	}
	if cond.Op == OpNOT && len(cond.Children) != 1 { // NOT negates exactly one subtree
		errs = append(errs, &ValidationError{Path: path,
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
		return ValidationErrors{{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("unknown field %q", cond.Field), Suggestions: c.suggestFields(cond.Field)}}
	}

	// 2) Capability gates: the field must be exposed and usable in a filter.
	if !f.EffectiveQueryable() {
		return ValidationErrors{{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("field %q is excluded from queries", cond.Field)}}
	}
	if !f.EffectiveFilterable() {
		return ValidationErrors{{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("field %q is not filterable", cond.Field)}}
	}

	// 3) Operator must be known, permitted for this field, and satisfy the
	//    text-search / regex policies.
	if !isKnownOperator(cond.Operator) {
		return ValidationErrors{{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("unknown operator %q", cond.Operator)}}
	}
	if !f.AllowsOperator(cond.Operator) {
		return ValidationErrors{{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q not allowed on field %q (allowed: %s)",
				cond.Operator, cond.Field, joinOperators(f.EffectiveOperators()))}}
	}
	// Text-search operators on a string/enum field require the searchable flag.
	if (f.Type == FieldString || f.Type == FieldEnum) && isTextSearchOperator(cond.Operator) && !f.EffectiveSearchable() {
		errs = append(errs, &ValidationError{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("field %q is not searchable, so %q is not allowed", cond.Field, cond.Operator)})
	}
	// The denyRegexOn policy blocks regex on named fields (ReDoS / PII safety).
	if cond.Operator == OpRegex && c.regexDenied(cond.Field) {
		errs = append(errs, &ValidationError{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("regex is denied on field %q by policy", cond.Field)})
	}

	// 4) Value shape. Null operators take no value; everything else requires one.
	if isNullOperator(cond.Operator) {
		if cond.Value != nil {
			errs = append(errs, &ValidationError{Path: path, Field: cond.Field,
				Message: fmt.Sprintf("%q takes no value", cond.Operator)})
		}
		return errs // no further value checks apply to null operators
	}
	if cond.Value == nil {
		errs = append(errs, &ValidationError{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q requires a value", cond.Operator)})
		return errs
	}

	// The scalar/element type to check values against. For an array field the
	// relevant type is its element type (itemType), which is what `contains`
	// and `containsAll/Any` compare against.
	elemType := f.Type
	if f.Type == FieldArray {
		elemType = f.ItemType
		if elemType == "" {
			elemType = FieldString // sensible default element type
		}
	}

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

	if cond.Value.Kind != KindArray { // the value must actually be an array
		return ValidationErrors{{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q expects an array value, got %q", cond.Operator, cond.Value.Kind)}}
	}
	elems, _ := cond.Value.AsSlice() // safe: kind is array

	if cond.Operator == OpBetween && len(elems) != 2 { // BETWEEN is a 2-tuple
		errs = append(errs, &ValidationError{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("between requires exactly 2 values, got %d", len(elems))})
	}
	if cond.Operator != OpBetween && len(elems) == 0 { // in/contains* need at least one
		errs = append(errs, &ValidationError{Path: path, Field: cond.Field,
			Message: fmt.Sprintf("operator %q requires at least one value", cond.Operator)})
	}

	for i, e := range elems { // type-check each element against the element type
		if !elementKindOK(elemType, e) {
			errs = append(errs, &ValidationError{Path: fmt.Sprintf("%s.value[%d]", path, i), Field: cond.Field,
				Message: fmt.Sprintf("element %d is not a valid %s", i, elemType)})
			continue
		}
		if elemType == FieldEnum { // enum elements must be in-domain
			if s, ok := e.(string); ok && !f.hasEnumValue(s) {
				errs = append(errs, &ValidationError{Path: fmt.Sprintf("%s.value[%d]", path, i), Field: cond.Field,
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

	if !scalarKindOK(elemType, v) { // the value kind must match the (element) type
		return ValidationErrors{{Path: path, Field: f.Name,
			Message: fmt.Sprintf("value kind %q is not compatible with field type %q", v.Kind, elemType)}}
	}

	if elemType == FieldEnum { // enum scalar must be in-domain
		if s, ok := v.AsString(); ok && !f.hasEnumValue(s) {
			errs = append(errs, &ValidationError{Path: path, Field: f.Name,
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

// scalarKindOK reports whether a scalar Value's kind is compatible with a type.
func scalarKindOK(t FieldType, v *Value) bool {
	switch t {
	case FieldString:
		return v.Kind == KindString
	case FieldNumber:
		return v.Kind == KindNumber
	case FieldBoolean:
		return v.Kind == KindBoolean
	case FieldEnum:
		return v.Kind == KindEnum || v.Kind == KindString // enum accepts a bare string too
	case FieldDate:
		return v.Kind == KindDate || v.Kind == KindRelativeDate
	default:
		return false // arrays are never a scalar value
	}
}

// elementKindOK reports whether a raw JSON array element matches a scalar type.
// JSON decodes numbers as float64, strings as string, booleans as bool.
func elementKindOK(t FieldType, e any) bool {
	switch t {
	case FieldString, FieldEnum, FieldDate:
		_, ok := e.(string)
		return ok
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
		errs = append(errs, &ValidationError{Path: path, Field: f.Name,
			Message: fmt.Sprintf("value %g is below minimum %g", n, *f.Validators.Min)})
	}
	if f.Validators.Max != nil && n > *f.Validators.Max {
		errs = append(errs, &ValidationError{Path: path, Field: f.Name,
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
