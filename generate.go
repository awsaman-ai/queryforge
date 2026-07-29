package queryforge

import (
	"fmt"
	"sort"
	"time"
)

// Generator compiles a validated AST into one backend's query. It is the only
// plugin point that touches a specific database dialect; everything upstream
// (planner, validator) is backend-agnostic. Implementations must be pure
// functions of their inputs: no network, no AI, no hidden state.
type Generator interface {
	Backend() string                                                // stable id, e.g. "sql", "mongo"
	Generate(q *Query, c *Config, opts GenOptions) (*Result, error) // AST -> backend query
}

// GenOptions carries per-call generation inputs that are not part of the AST.
type GenOptions struct {
	Now time.Time // reference time for resolving relative dates; zero => time.Now()
}

// now returns the effective reference time (injected for tests, else wall clock).
func (o GenOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now
}

// Result is the compiled query in a backend-neutral envelope. SQL-like backends
// fill SQL+Args (parameterized); document/search backends fill Doc with their
// own structure. Warnings are non-fatal advisories (e.g. filtering a
// non-indexed field) that a caller may surface or ignore.
type Result struct {
	Backend  string   // which backend produced this result
	SQL      string   // parameterized SQL text (SQL backends)
	Args     []any    // bound arguments for the SQL placeholders, in order
	Doc      any      // query document for non-SQL backends (e.g. *MongoQuery)
	Warnings []string // advisories that do not block execution
}

// Registry maps backend ids to their generators. Registration is explicit (no
// magic auto-discovery), which keeps builds reproducible and enterprise review
// straightforward.
type Registry struct {
	gens map[string]Generator // backend id -> generator
}

// NewRegistry returns an empty generator registry.
func NewRegistry() *Registry {
	return &Registry{gens: make(map[string]Generator)}
}

// DefaultRegistry returns a registry pre-loaded with the MVP backends.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(SQLGenerator{})   // relational (Postgres dialect)
	r.Register(MongoGenerator{}) // document store
	return r
}

// Register adds (or replaces) a generator for its backend id.
func (r *Registry) Register(g Generator) { r.gens[g.Backend()] = g }

// Get looks up a generator by backend id.
func (r *Registry) Get(backend string) (Generator, bool) {
	g, ok := r.gens[backend]
	return g, ok
}

// Backends lists the registered backend ids in sorted order (stable output).
func (r *Registry) Backends() []string {
	out := make([]string, 0, len(r.gens))
	for b := range r.gens {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// --- shared helpers used by every generator ---

// effectiveLimit returns the limit to apply: the AST's explicit value, else the
// config default (0 means "no limit").
func effectiveLimit(q *Query, c *Config) int {
	if q.Limit != nil {
		return *q.Limit
	}
	return c.Defaults.Limit
}

// effectiveOffset returns the offset to apply (0 when unset).
func effectiveOffset(q *Query) int {
	if q.Offset != nil {
		return *q.Offset
	}
	return 0
}

// projectionFields returns the logical field names to return: the explicit
// select list, or all returnable fields when select is empty. The second return
// value is false when the projection is "everything" (SELECT *).
//
// A field marked returnable:false must never reach the caller. The wide form
// ("SELECT *", or an unprojected Mongo find) would return every physical column
// including the hidden ones, so it is only safe when the config hides nothing.
// As soon as one field is non-returnable we emit an explicit allow-list of the
// returnable fields instead — the config's exclusion has to hold on the default
// path, not just when the model happens to request a select list.
func projectionFields(q *Query, c *Config) ([]string, bool) {
	if len(q.Select) > 0 {
		return q.Select, true // explicit projection (validated against returnable)
	}

	// Walk the config in declaration order so the emitted column list is stable.
	returnable := make([]string, 0, len(c.Fields)) // the allow-list we may emit
	hidden := false                                // true once any field opts out
	for _, f := range c.Fields {
		if f.EffectiveReturnable() {
			returnable = append(returnable, f.Name)
			continue
		}
		hidden = true
	}

	if !hidden {
		return nil, false // nothing to hide: the backend's "all fields" form is safe
	}
	return returnable, true
}

// orderedChildren returns a copy of a logical node's children reordered so that
// predicates on indexed fields come first, then higher-priority fields. AND/OR
// are commutative, so this is semantics-preserving; the sort is stable, so
// equal-ranked predicates keep their original order (deterministic output).
func orderedChildren(c *Config, children []*Condition) []*Condition {
	out := make([]*Condition, len(children))
	copy(out, children)
	sort.SliceStable(out, func(i, j int) bool {
		iRank, iPrio := condSortKey(c, out[i])
		jRank, jPrio := condSortKey(c, out[j])
		if iRank != jRank {
			return iRank < jRank // indexed (rank 0) sorts before non-indexed (rank 1)
		}
		return iPrio > jPrio // within the same index class, higher priority first
	})
	return out
}

// condSortKey derives the ordering key for a condition: whether its field is
// indexed (0/1) and its priority. Logical nodes get the neutral (1, 0) key.
func condSortKey(c *Config, cond *Condition) (indexedRank int, priority int) {
	// Caller-supplied scope predicates sort ahead of everything. They are the
	// most selective filter in the query (one subscription out of all of them)
	// and sit on tenant-style columns that are essentially always indexed, even
	// though the config need not declare them. Putting them first also makes the
	// emitted query read the way it behaves: the scope, then the question.
	if cond.Scoped {
		return -1, 0
	}
	if cond.Type == CondComparison {
		if f, ok := c.FieldByName(cond.Field); ok {
			if f.Indexed {
				return 0, f.Priority
			}
			return 1, f.Priority
		}
	}
	return 1, 0
}

// collectIndexWarnings walks the filter tree and flags each predicate on a
// non-indexed field. These are hints (the requirement's "soft warning"), never
// hard failures.
func collectIndexWarnings(q *Query, c *Config) []string {
	var warnings []string
	seen := make(map[string]bool) // de-duplicate per field
	var walk func(cond *Condition)
	walk = func(cond *Condition) {
		if cond == nil {
			return
		}
		if cond.Type == CondComparison {
			if f, ok := c.FieldByName(cond.Field); ok && !f.Indexed && !seen[cond.Field] {
				seen[cond.Field] = true
				warnings = append(warnings, fmt.Sprintf("filtering on non-indexed field %q may be slow", cond.Field))
			}
			return
		}
		for _, ch := range cond.Children {
			walk(ch)
		}
	}
	walk(q.Filter)
	return warnings
}

// resolveRelative resolves a relative_date (unit, amount) against a reference
// time into an absolute time. A negative amount is in the past.
func resolveRelative(now time.Time, unit string, amount int) time.Time {
	switch unit {
	case "minute":
		return now.Add(time.Duration(amount) * time.Minute)
	case "hour":
		return now.Add(time.Duration(amount) * time.Hour)
	case "day":
		return now.AddDate(0, 0, amount)
	case "week":
		return now.AddDate(0, 0, amount*7)
	case "month":
		return now.AddDate(0, amount, 0)
	case "year":
		return now.AddDate(amount, 0, 0)
	default:
		return now // unknown unit: fall back to now (validator restricts units upstream)
	}
}
