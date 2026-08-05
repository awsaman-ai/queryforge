package queryforge

import "strings"

// ValueCase is a field's declared value-case rule: the letter case the string
// values for that field must take in the compiled query.
//
// The rule lives entirely on the generation side of the pipeline. The AST, the
// prompt, the enum domain and validation all keep the config's own spelling —
// so a config stays readable and round-trippable — and only the final SQL
// argument or Mongo filter value carries the physical case.
type ValueCase string

const (
	// CaseAsIs is the zero value: the string reaches the query exactly as the
	// AST carried it. This is what an omitted "valueCase" key means.
	CaseAsIs ValueCase = ""
	// CaseLower folds values to lower case (strings.ToLower).
	CaseLower ValueCase = "lower"
	// CaseUpper folds values to upper case (strings.ToUpper).
	CaseUpper ValueCase = "upper"
)

// validValueCase reports whether vc is one of the three accepted settings. Used
// by config load so a typo ("uppercase", "UPPER") fails at load rather than
// degrading into a silent no-op at query time.
func validValueCase(vc ValueCase) bool {
	switch vc {
	case CaseAsIs, CaseLower, CaseUpper:
		return true
	default:
		return false
	}
}

// carriesStrings reports whether a field's values are strings, and so whether a
// case rule can mean anything for it. Arrays delegate to their element type; an
// array with no declared itemType defaults to string elsewhere in the library,
// so it counts here too.
func (f Field) carriesStrings() bool {
	t := f.Type
	if t == FieldArray {
		t = f.ItemType
		if t == "" {
			t = FieldString
		}
	}
	return t == FieldString || t == FieldEnum
}

// valueCaseFor returns the case rule registered for a logical field name.
//
// An unregistered name yields CaseAsIs rather than an error: the generators
// call this for every predicate, including the scope filters an application
// injects on columns that need not appear in the config at all, and those must
// keep passing through untouched.
func (c *Config) valueCaseFor(fieldName string) ValueCase {
	f, ok := c.fieldByName[fieldName]
	if !ok {
		return CaseAsIs
	}
	return f.ValueCase
}

// apply folds a single string according to the rule. CaseAsIs returns the input
// unchanged, so callers can apply it unconditionally.
func (vc ValueCase) apply(s string) string {
	switch vc {
	case CaseLower:
		return strings.ToLower(s)
	case CaseUpper:
		return strings.ToUpper(s)
	default:
		return s
	}
}

// applyAny folds v when it is a string and returns everything else untouched.
// Array elements arrive as `any` (numbers, bools, already-parsed dates), and a
// case rule must never reshape a non-string element.
func (vc ValueCase) applyAny(v any) any {
	if s, ok := v.(string); ok {
		return vc.apply(s)
	}
	return v
}

// applyElems folds every string element of an array value.
//
// It allocates a new slice rather than writing through the one it was given:
// AsSlice hands back the payload owned by the caller's AST, and the library's
// contract is that compiling a query never mutates the AST it was handed. When
// there is no rule the original slice is returned as-is, so the ordinary path
// allocates nothing.
func (vc ValueCase) applyElems(elems []any) []any {
	if vc == CaseAsIs {
		return elems
	}
	out := make([]any, len(elems))
	for i, e := range elems {
		out[i] = vc.applyAny(e)
	}
	return out
}
