// Package policy enforces the cross-field business rules a config declares in
// Policy.Requires — the checks that run only after an AST is already structurally
// valid, and whose answer is "this query is legal but would not make sense".
package policy

import (
	"fmt"
	"strings"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
)

// PolicyViolationError reports that an AST is structurally legal — every
// field, operator and value already passed Validate's per-field checks — but
// breaks a business rule declared in Policy.Requires: the question filters a
// field that only makes sense alongside another one it did not also filter
// (e.g. passport expiry without a country).
//
// It is deliberately its own type, not a ValidationError, because the two
// answer different questions. A ValidationError means the AST cannot be
// expressed in this config's vocabulary at all. A PolicyViolationError means
// it CAN be expressed, and was, but the result would not make business sense
// — closer to UnsupportedRequestError's "the model declined on purpose" than
// to an ordinary validation finding. Callers that want to tell the two apart
// (to show a different message, or skip the repair budget) can with
// errors.As; callers that don't can treat both as "no query" and move on.
type PolicyViolationError struct {
	Field            string   // the field whose presence triggered the rule
	RequireAlsoOneOf []string // the field(s) the rule needed alongside it
	Message          string   // human-readable explanation (config's own, or a generated default)
}

// Error renders the violation.
func (e *PolicyViolationError) Error() string { return e.Message }

// Check enforces c.Policy.Requires against the fields actually
// referenced in an AST's filter tree. It is meaningful only once the AST has
// already passed every other validation rule — checking business sense on a
// malformed AST would just be confusing — so Validate calls it last.
//
// It reports at most one violation: the first rule, in declared order, that
// fires and finds nothing in RequireAlsoOneOf. That mirrors how a person would
// read the rules top to bottom and stop at the first real problem, rather
// than dumping every rule's verdict on the caller at once.
func Check(filter *ast.Condition, c *config.Config) *PolicyViolationError {
	if len(c.Policy.Requires) == 0 || filter == nil {
		return nil
	}
	used := filterFieldOperators(filter)

	for _, rule := range c.Policy.Requires {
		ops, triggered := used[rule.When.Field]
		if !triggered {
			continue
		}
		if len(rule.When.Operators) > 0 && !anyOperatorIn(ops, rule.When.Operators) {
			continue // this field was filtered, but not with a triggering operator
		}
		if anyFieldIn(used, rule.RequireAlsoOneOf) {
			continue // satisfied — at least one required companion field is present
		}
		return &PolicyViolationError{
			Field:            rule.When.Field,
			RequireAlsoOneOf: rule.RequireAlsoOneOf,
			Message:          policyMessage(rule),
		}
	}
	return nil
}

// policyMessage returns the rule's own message, or a generated one naming the
// two sides of the rule so a caller never sees a blank explanation just
// because the config author left Message unset.
func policyMessage(r config.FieldRequirement) string {
	if r.Message != "" {
		return r.Message
	}
	return fmt.Sprintf("filtering %q also requires filtering one of: %s",
		r.When.Field, strings.Join(r.RequireAlsoOneOf, ", "))
}

// filterFieldOperators walks a filter tree and collects, for every field it
// compares, the set of operators used against it anywhere in the tree — AND,
// OR, NOT, or nested, since a rule cares only "was this field filtered at
// all", not where.
func filterFieldOperators(cond *ast.Condition) map[string]map[ast.Operator]bool {
	used := make(map[string]map[ast.Operator]bool)
	var walk func(*ast.Condition)
	walk = func(c *ast.Condition) {
		if c == nil {
			return
		}
		if c.Type == ast.CondComparison {
			if used[c.Field] == nil {
				used[c.Field] = make(map[ast.Operator]bool)
			}
			used[c.Field][c.Operator] = true
			return
		}
		for _, child := range c.Children {
			walk(child)
		}
	}
	walk(cond)
	return used
}

// anyOperatorIn reports whether any of ops appears in used.
func anyOperatorIn(used map[ast.Operator]bool, ops []string) bool {
	for _, op := range ops {
		if used[ast.Operator(op)] {
			return true
		}
	}
	return false
}

// anyFieldIn reports whether any of fields was filtered at all (any operator).
func anyFieldIn(used map[string]map[ast.Operator]bool, fields []string) bool {
	for _, f := range fields {
		if _, ok := used[f]; ok {
			return true
		}
	}
	return false
}
