package queryforge

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
)

// Request-level advisories (QA audit 2026-11-20, F3 and P1).
//
// Both are WARNINGS, never errors: each describes a query that compiled
// correctly from the model's AST but that the caller probably does not want to
// trust blindly. They ride in TranslateResult.Warnings, which every SDK already
// carries, so no protocol change is needed.

// mutationVerbs are imperative verbs that ask for a change to data. QueryForge
// can only read: the AST has no write node, so such a request can never be
// executed — but a model asked to "close every open ticket" may still emit a
// filter, and that filter can describe the wrong population (the audit saw it
// select tickets that were already CLOSED). A deterministic check beats
// hoping the model refuses.
var mutationVerbs = map[string]bool{
	"close": true, "reopen": true, "re-open": true, "delete": true, "remove": true,
	"update": true, "mark": true, "cancel": true, "approve": true, "reject": true,
	"archive": true, "unarchive": true, "assign": true, "reassign": true, "set": true,
	"change": true, "modify": true, "edit": true, "merge": true, "refund": true,
	"void": true, "purge": true, "drop": true, "truncate": true, "insert": true,
	"create": true, "add": true, "escalate": true, "resolve": true, "flag": true,
	"unflag": true, "deactivate": true, "activate": true, "suspend": true, "ban": true,
}

// politePrefixes are stripped (repeatedly) before the first-word test, so
// "please close …" and "can you delete …" are caught like "close …".
var politePrefixes = []string{
	"please ", "kindly ", "can you ", "could you ", "would you ", "will you ",
	"i want to ", "i would like to ", "i'd like to ", "i need to ", "go ahead and ",
	"let's ", "lets ", "just ", "now ", "also ",
}

// mutationVerb returns the write verb a request leads with, or "". Only the
// opening verb (and one after "then"/"and then") counts: a read question that
// merely mentions "closed tickets" or "tickets to close" must not warn.
func mutationVerb(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	for changed := true; changed; {
		changed = false
		for _, p := range politePrefixes {
			if strings.HasPrefix(t, p) {
				t = strings.TrimSpace(strings.TrimPrefix(t, p))
				changed = true
			}
		}
	}
	if v := firstWord(t); mutationVerbs[v] && !isNounUse(v, t) {
		return v
	}
	for _, sep := range []string{" and then ", " then "} {
		if i := strings.Index(t, sep); i >= 0 {
			if v := firstWord(t[i+len(sep):]); mutationVerbs[v] {
				return v
			}
		}
	}
	return ""
}

// isNounUse screens the one opening word that is also a common noun in read
// questions: "set of orders …", "drop in sales …", "change in price …".
func isNounUse(verb, t string) bool {
	switch verb {
	case "set", "drop", "change", "flag":
		rest := strings.TrimSpace(strings.TrimPrefix(t, verb))
		return strings.HasPrefix(rest, "of ") || strings.HasPrefix(rest, "in ")
	}
	return false
}

func firstWord(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ';' || r == ':' })
	if end < 0 {
		end = len(s)
	}
	return strings.Trim(s[:end], `"'.!?`)
}

func mutationAdvisory(text string) []string {
	v := mutationVerb(text)
	if v == "" {
		return nil
	}
	return []string{fmt.Sprintf(
		"the request asks to %q, which changes data; QueryForge only reads. This query selects rows from the "+
			"descriptive terms in the question and does not perform that action — check the filter describes the rows "+
			"you meant before acting on the result", v)}
}

// contradictionAdvisories reports AND-reachable equals predicates that pin one
// scalar field to two different values. That filter can never match a row, and
// the caller would otherwise receive a confidently empty result (P1: "perishable
// electronics" -> category = PERISHABLE AND category = ELECTRONICS).
//
// Only conditions reachable through AND alone are compared; under OR/NOT the two
// values may legitimately both appear. Array fields are skipped: "tags contains
// a" and "tags contains b" can both hold on one row.
func contradictionAdvisories(q *ast.Query, c *config.Config) []string {
	if q == nil || q.Filter == nil {
		return nil
	}
	seen := map[string]*ast.Value{}
	var out []string
	var walk func(n *ast.Condition)
	walk = func(n *ast.Condition) {
		if n == nil {
			return
		}
		switch n.Type {
		case ast.CondLogical:
			if n.Op != ast.OpAND {
				return
			}
			for _, ch := range n.Children {
				walk(ch)
			}
		case ast.CondComparison:
			if n.Operator != ast.OpEquals || n.Value == nil || n.Value.Kind == ast.KindArray {
				return
			}
			if f, ok := c.FieldByName(n.Field); !ok || f.Type == config.FieldArray {
				return
			}
			prev, dup := seen[n.Field]
			if !dup {
				seen[n.Field] = n.Value
				return
			}
			if !sameLiteral(prev, n.Value) {
				out = append(out, fmt.Sprintf(
					"the filter requires %q to equal both %s and %s at once, so it can never match a row — "+
						"the question may be self-contradictory (e.g. two values joined with 'and' where 'or' was meant)",
					n.Field, literal(prev), literal(n.Value)))
			}
		}
	}
	walk(q.Filter)
	return out
}

func sameLiteral(a, b *ast.Value) bool {
	if sa, ok := a.V.(string); ok {
		if sb, ok := b.V.(string); ok {
			return strings.EqualFold(sa, sb) // caseInsensitive fields must not false-positive
		}
	}
	return reflect.DeepEqual(a.V, b.V)
}

func literal(v *ast.Value) string { return fmt.Sprintf("%v", v.V) }
