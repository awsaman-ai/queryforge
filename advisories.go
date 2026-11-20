package queryforge

import (
	"fmt"
	"reflect"
	"sort"
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
	// BUG-07 (QA audit v1.3.0): domain write verbs the first list missed. The
	// ones that are also common nouns or modifiers (ship, bill, charge, recall)
	// are screened by isNounUse; anything else domain-specific belongs in the
	// config's policy.mutationVerbs, since write verbs are inherently per-domain.
	"dispatch": true, "restock": true, "quarantine": true, "waive": true, "reroute": true,
	"unsubscribe": true, "promote": true, "demote": true, "recall": true, "ship": true,
	"bill": true, "charge": true, "reserve": true, "transfer": true, "notify": true,
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
func mutationVerb(text string, extra []string) string {
	isVerb := func(v string) bool { return mutationVerbs[v] || containsFold(extra, v) }
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
	if v := firstWord(t); isVerb(v) && !isNounUse(v, t) {
		return v
	}
	for _, sep := range []string{" and then ", " then "} {
		if i := strings.Index(t, sep); i >= 0 {
			if v := firstWord(t[i+len(sep):]); isVerb(v) && !isNounUse(v, t[i+len(sep):]) {
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
	case "ship", "bill", "charge", "recall", "transfer", "reserve", "promote":
		// "ship date of…", "bill totals over…", "charge amount above…" read the
		// word as a noun/modifier: it is followed by a column-ish word rather
		// than the object of an action.
		rest := strings.TrimSpace(strings.TrimPrefix(t, verb))
		w := firstWord(rest)
		switch w {
		case "date", "dates", "to", "from", "via", "by", "method", "mode", "address", "status", "cost",
			"costs", "fee", "fees", "time", "times", "amount", "amounts", "total", "totals", "id", "ids",
			"number", "count", "rate", "rates", "type", "types", "of", "in", "is", "are", "was", "were":
			return true
		}
	}
	return false
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
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

func mutationAdvisory(text string, extraVerbs []string) []string {
	v := mutationVerb(text, extraVerbs)
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
				if separateElement(n.Distinct, ch, c) {
					continue // matched against its own array element
				}
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

// separateElement reports whether cond is a direct comparison child of a distinct
// AND on an array-of-sub-documents field. Such a predicate is matched against its
// OWN array element, so it cannot conflict with a sibling on the same field:
// "a DEMO activity and a different CALL activity" is satisfiable even though one
// element cannot be both. The contradiction checks must skip these, or they warn
// about exactly the query the distinct marker was added to express.
func separateElement(parentDistinct bool, cond *ast.Condition, c *config.Config) bool {
	if !parentDistinct || cond == nil || cond.Type != ast.CondComparison {
		return false
	}
	f, ok := c.FieldByName(cond.Field)
	return ok && (f.ElemMatch != "" || f.NestedPath != "")
}

// numBound is one side of a numeric range: its value and whether it is exclusive.
type numBound struct {
	v         float64
	exclusive bool
	set       bool
}

// rangeContradictionAdvisories (BUG-06, QA audit v1.3.0) reports AND-reachable
// numeric bounds on one field that cannot overlap — "qty > 100 AND qty < 10" —
// the range counterpart of contradictionAdvisories' equals check. The tightest
// lower bound (gt/gte/between-low) is compared with the tightest upper bound
// (lt/lte/between-high); touching exclusive bounds (>5 and <5, >=5 and <5)
// are empty too. Only plain number values on non-array fields participate.
func rangeContradictionAdvisories(q *ast.Query, c *config.Config) []string {
	if q == nil || q.Filter == nil {
		return nil
	}
	type span struct{ lo, hi numBound }
	spans := map[string]*span{}
	var order []string
	setLo := func(sp *span, v float64, excl bool) {
		if !sp.lo.set || v > sp.lo.v || (v == sp.lo.v && excl) {
			sp.lo = numBound{v, excl, true}
		}
	}
	setHi := func(sp *span, v float64, excl bool) {
		if !sp.hi.set || v < sp.hi.v || (v == sp.hi.v && excl) {
			sp.hi = numBound{v, excl, true}
		}
	}
	var walk func(n *ast.Condition)
	walk = func(n *ast.Condition) {
		if n == nil {
			return
		}
		if n.Type == ast.CondLogical {
			if n.Op == ast.OpAND {
				for _, ch := range n.Children {
					if separateElement(n.Distinct, ch, c) {
						continue // matched against its own array element
					}
					walk(ch)
				}
			}
			return
		}
		if n.Type != ast.CondComparison || n.Value == nil {
			return
		}
		if f, ok := c.FieldByName(n.Field); !ok || f.Type == config.FieldArray {
			return
		}
		get := func() *span {
			if spans[n.Field] == nil {
				spans[n.Field] = &span{}
				order = append(order, n.Field)
			}
			return spans[n.Field]
		}
		switch n.Operator {
		case ast.OpGt, ast.OpGte, ast.OpLt, ast.OpLte:
			x, ok := n.Value.V.(float64)
			if n.Value.Kind != ast.KindNumber || !ok {
				return
			}
			sp := get()
			switch n.Operator {
			case ast.OpGt:
				setLo(sp, x, true)
			case ast.OpGte:
				setLo(sp, x, false)
			case ast.OpLt:
				setHi(sp, x, true)
			case ast.OpLte:
				setHi(sp, x, false)
			}
		case ast.OpBetween:
			arr, ok := n.Value.V.([]any)
			if n.Value.Kind != ast.KindArray || !ok || len(arr) != 2 {
				return
			}
			lo, ok1 := arr[0].(float64)
			hi, ok2 := arr[1].(float64)
			if !ok1 || !ok2 {
				return
			}
			sp := get()
			setLo(sp, lo, false)
			setHi(sp, hi, false)
		}
	}
	walk(q.Filter)
	var out []string
	for _, field := range order {
		sp := spans[field]
		if !sp.lo.set || !sp.hi.set {
			continue
		}
		if sp.lo.v > sp.hi.v || (sp.lo.v == sp.hi.v && (sp.lo.exclusive || sp.hi.exclusive)) {
			out = append(out, fmt.Sprintf(
				"the filter requires %q to be %s %v and %s %v at once, so it can never match a row — "+
					"the question may be self-contradictory (e.g. two thresholds joined with 'and' where 'or' was meant)",
				field, boundWord(sp.lo, true), sp.lo.v, boundWord(sp.hi, false), sp.hi.v))
		}
	}
	return out
}

func boundWord(b numBound, lower bool) string {
	switch {
	case lower && b.exclusive:
		return "greater than"
	case lower:
		return "at least"
	case b.exclusive:
		return "less than"
	}
	return "at most"
}

// Cues that fix a relative date's direction in the request text.
var (
	forwardCues = []string{"next ", "upcoming", "coming ", "from now", "tomorrow", "ahead", "in the future", "due in", "within the next", "expiring in", "expire in"}
	pastCues    = []string{"last ", "past ", "previous", "ago", "yesterday", "recent", "so far", "earlier", "over the last"}
)

func hasCue(t string, cues []string) bool {
	for _, c := range cues {
		if strings.Contains(t, c) {
			return true
		}
	}
	return false
}

// dateDirectionAdvisories (BUG-04, QA audit v1.3.0) flags a relative-date value
// that points the opposite way from the request: "expiring next week" compiled
// to a period that ends today, i.e. the PAST week. The direction is a model
// reading, and a silently inverted window returns plausible-looking wrong rows,
// so it is checked deterministically against cue words in the text. When the
// text carries cues of BOTH directions ("next week vs last week") nothing is
// asserted, because the AST legitimately holds both.
func dateDirectionAdvisories(text string, q *ast.Query) []string {
	if q == nil || q.Filter == nil {
		return nil
	}
	t := " " + strings.ToLower(text) + " "
	fwd, past := hasCue(t, forwardCues), hasCue(t, pastCues)
	if fwd == past { // no cue at all, or contradictory cues: nothing to assert
		return nil
	}
	var out []string
	var walk func(n *ast.Condition)
	walk = func(n *ast.Condition) {
		if n == nil {
			return
		}
		for _, ch := range n.Children {
			walk(ch)
		}
		if n.Type != ast.CondComparison || n.Value == nil {
			return
		}
		lo, hi, ok := 0, 0, false
		switch n.Value.Kind {
		case ast.KindPeriod:
			lo, hi, ok = n.Value.From, n.Value.To, true
		case ast.KindRelativeDate:
			lo, hi, ok = n.Value.Amount, n.Value.Amount, true
		}
		if !ok {
			return
		}
		switch {
		case fwd && lo <= 0 && hi <= 0 && (lo < 0 || hi < 0):
			out = append(out, fmt.Sprintf(
				"the request looks forward in time (\"next\"/\"upcoming\"), but the date filter on %q covers the PAST "+
					"(%d..%d %s relative to now) — the direction may be inverted", n.Field, lo, hi, n.Value.Unit))
		case past && lo >= 0 && hi >= 0 && (lo > 0 || hi > 0):
			out = append(out, fmt.Sprintf(
				"the request looks back in time (\"last\"/\"past\"), but the date filter on %q covers the FUTURE "+
					"(%d..%d %s relative to now) — the direction may be inverted", n.Field, lo, hi, n.Value.Unit))
		}
	}
	walk(q.Filter)
	return out
}

// differentElementCues are request phrasings that ask for two array predicates
// to hold on DIFFERENT elements of the same array.
var differentElementCues = []string{"a different ", "another ", "an other ", "other than that", "a second ", "separate ", "different one", "different batch", "different item", "different entry", "different "}

// distinctElementAdvisories (BUG-02, QA audit v1.3.0) warns when the request asks
// for a "different" array element but the AST ANDs several predicates on one
// elemMatch/nestedPath array without a distinct AND: the generators then fold
// them into a SAME-element match — the opposite of what was asked, and silent.
func distinctElementAdvisories(text string, q *ast.Query, c *config.Config) []string {
	if q == nil || q.Filter == nil || !hasCue(" "+strings.ToLower(text), differentElementCues) {
		return nil
	}
	arrayOf := func(field string) string {
		f, ok := c.FieldByName(field)
		if !ok {
			return ""
		}
		if f.ElemMatch != "" {
			return f.ElemMatch
		}
		return f.NestedPath
	}
	var out []string
	var walk func(n *ast.Condition)
	walk = func(n *ast.Condition) {
		if n == nil || n.Type != ast.CondLogical {
			return
		}
		if n.Op == ast.OpAND && !n.Distinct {
			count := map[string]int{}
			for _, ch := range n.Children {
				// A child is "one predicate group on an array" when it is a
				// comparison on an array field; nested ANDs are folded by the
				// generator too, so descend into them via their own walk.
				if ch != nil && ch.Type == ast.CondComparison {
					if a := arrayOf(ch.Field); a != "" {
						count[a]++
					}
				}
			}
			for a, n2 := range count {
				if n2 > 1 {
					out = append(out, fmt.Sprintf(
						"the request mentions a DIFFERENT element, but %d predicates on array %q are AND-ed without \"distinct\", "+
							"so they compile to a single same-element match — the question may mean separate elements", n2, a))
				}
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(q.Filter)
	sort.Strings(out)
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
