// Package valuetype answers one question the validator and every generator have
// to answer identically: what declared type is a literal in this condition read
// as?
//
// It is its own package rather than a corner of internal/config because the
// answer depends on both the config (a field's declared type, or its element
// type for arrays) and the AST (a value's own kind, for the one case where no
// declared type exists), and nothing below both of those can hold it.
package valuetype

import (
	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
)

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

// ScalarTypeOf returns the declared type a field's scalar values are read as.
// For an array field that is its element type, which is what `contains` and
// `containsAll/Any` compare with; itemType defaults to string when omitted.
func ScalarTypeOf(f *config.Field) config.FieldType {
	if f.Type != config.FieldArray {
		return f.Type
	}
	if f.ItemType == "" {
		return config.FieldString
	}
	return f.ItemType
}

// CondScalarType resolves the type a condition's literals are read as.
//
// Normally that is what the config declares. A predicate on an UNDECLARED field
// can still reach a generator, by one route only: an application-injected scope
// filter, which is spliced in after validation precisely so a tenant key need
// not be part of the model's vocabulary. There the config has nothing to say,
// and the kind tag is not a guess — scope.go derived it from a real Go value —
// so that is what the type comes from.
func CondScalarType(c *config.Config, cond *ast.Condition) config.FieldType {
	if f, ok := c.FieldByName(cond.Field); ok {
		return ScalarTypeOf(f)
	}
	return TypeForKind(cond.Value)
}

// TypeForKind maps a value's own kind onto the field type that reads it, for
// the one case where no declared type exists.
func TypeForKind(v *ast.Value) config.FieldType {
	if v == nil {
		return config.FieldString // isNull/isNotNull: nothing is read
	}
	switch v.Kind {
	case ast.KindNumber:
		return config.FieldNumber
	case ast.KindBoolean:
		return config.FieldBoolean
	case ast.KindDate, ast.KindRelativeDate:
		return config.FieldDate
	default: // string, enum, array (elements carry their own Go types)
		return config.FieldString
	}
}
