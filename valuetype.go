package queryforge

import "time"

// Which description of a literal wins: the config, or the model.
//
// A model-produced AST describes each literal twice — with the "kind" tag it
// chose, and with the payload itself. The config describes it a third time, and
// authoritatively, because it declares the field's type. The tag is therefore
// redundant, and being redundant it is the part that goes wrong. The reported
// failure: a config declaring
//
//	{"name":"Status","type":"string"}
//
// asked for "delivered orders". The model copied the prompt's example predicate
// tag and all, emitting {"kind":"enum","v":"DELIVERED"}. Every part of that a
// database would care about is correct — real field, legal operator, a string
// literal for a string column — but the pipeline read the tag first and failed
// the whole query.
//
// The same misplaced trust ran the other way too, and that half was worse:
// {"kind":"number","v":"42"} on a number field passed validation on the
// strength of its tag, then AsFloat found text, dropped it, and bound 0. A
// wrong answer returned confidently, which is the one outcome this library
// exists to prevent.
//
// So the tag decides nothing. The config says what type a field is; the payload
// says what the model actually produced; the two are compared directly, in the
// validator (scalarKindOK) and again in each generator (sqlBuilder.value,
// mongoScalar). The tag survives only where it is the sole description
// available: relative_date carries no payload, just unit and amount.
//
// This also leaves the model's AST untouched. An earlier attempt normalized the
// tags in place and tripped TestScopeIsSafeForConcurrentUse — GenerateFrom
// takes an AST the caller owns and may be sharing across goroutines, so the
// only safe repair is one that writes nothing.

// scalarTypeOf returns the declared type a field's scalar values are read as.
// For an array field that is its element type, which is what `contains` and
// `containsAll/Any` compare with; itemType defaults to string when omitted.
func scalarTypeOf(f *Field) FieldType {
	if f.Type != FieldArray {
		return f.Type
	}
	if f.ItemType == "" {
		return FieldString
	}
	return f.ItemType
}

// condScalarType resolves the type a condition's literals are read as.
//
// Normally that is what the config declares. A predicate on an UNDECLARED field
// can still reach a generator, by one route only: an application-injected scope
// filter, which is spliced in after validation precisely so a tenant key need
// not be part of the model's vocabulary. There the config has nothing to say,
// and the kind tag is not a guess — scope.go derived it from a real Go value —
// so that is what the type comes from.
func condScalarType(c *Config, cond *Condition) FieldType {
	if f, ok := c.FieldByName(cond.Field); ok {
		return scalarTypeOf(f)
	}
	return typeForKind(cond.Value)
}

// typeForKind maps a value's own kind onto the field type that reads it, for
// the one case where no declared type exists.
func typeForKind(v *Value) FieldType {
	if v == nil {
		return FieldString // isNull/isNotNull: nothing is read
	}
	switch v.Kind {
	case KindNumber:
		return FieldNumber
	case KindBoolean:
		return FieldBoolean
	case KindDate, KindRelativeDate:
		return FieldDate
	default: // string, enum, array (elements carry their own Go types)
		return FieldString
	}
}

// isDateString reports whether s parses under one of the layouts the generators
// accept. Sharing it with parseDate is what keeps the validator from admitting a
// date the Mongo generator would then hand back as a bare string.
func isDateString(s string) bool {
	_, ok := parseDate(s).(time.Time)
	return ok
}
