// Package testutil holds the small AST and config builders shared by the test
// suites of the internal packages.
//
// It deliberately lives in a regular (non-_test.go) file so that any package's
// test files can import it. Before the layered-package split these helpers were
// unexported functions duplicated at the top of validate_test.go and
// config_test.go; hoisting them here keeps a single source of truth now that
// the tests that use them live in different packages.
//
// This package is internal and exists only to serve tests — nothing in the
// shipping library imports it.
package testutil

import (
	"testing"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
)

// --- tiny builders keep the tests readable ---

// Comp builds a comparison condition.
func Comp(field string, op ast.Operator, v *ast.Value) *ast.Condition {
	return &ast.Condition{Type: ast.CondComparison, Field: field, Operator: op, Value: v}
}

// And builds a logical AND over children.
func And(children ...*ast.Condition) *ast.Condition {
	return &ast.Condition{Type: ast.CondLogical, Op: ast.OpAND, Children: children}
}

// Or builds a logical OR over children.
func Or(children ...*ast.Condition) *ast.Condition {
	return &ast.Condition{Type: ast.CondLogical, Op: ast.OpOR, Children: children}
}

// Not builds a logical NOT over a single child.
func Not(child *ast.Condition) *ast.Condition {
	return &ast.Condition{Type: ast.CondLogical, Op: ast.OpNOT, Children: []*ast.Condition{child}}
}

// VEnum builds an enum value.
func VEnum(s string) *ast.Value { return &ast.Value{Kind: ast.KindEnum, V: s} }

// VStr builds a string value.
func VStr(s string) *ast.Value { return &ast.Value{Kind: ast.KindString, V: s} }

// VBool builds a boolean value.
func VBool(b bool) *ast.Value { return &ast.Value{Kind: ast.KindBoolean, V: b} }

// VNum builds a number value.
func VNum(n float64) *ast.Value { return &ast.Value{Kind: ast.KindNumber, V: n} }

// VArr builds an array value.
func VArr(items ...any) *ast.Value { return &ast.Value{Kind: ast.KindArray, V: items} }

// VRel builds a relative-date value.
func VRel(u string, a int) *ast.Value {
	return &ast.Value{Kind: ast.KindRelativeDate, Unit: u, Amount: a}
}

// Single wraps one comparison into a Query for the adversarial tables.
func Single(cmp *ast.Condition) *ast.Query {
	q := ast.NewQuery("Order")
	q.Filter = cmp
	return q
}

// MustParse parses a config JSON document or fails the test.
func MustParse(t *testing.T, js string) *config.Config {
	t.Helper()
	c, err := config.ParseConfig([]byte(js))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return c
}
