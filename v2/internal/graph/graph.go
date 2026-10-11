// Package graph is the schema graph: entities as nodes, configured
// relationships as edges, and deterministic join-path search between them.
//
// Section 13 of the brief: "The graph, not an LLM, determines how those
// entities can connect." Section 16.6 makes the configured relationships an
// allowlist, so there is no other source of edges — a join the config does not
// declare cannot be produced here, however plausible it looks upstream.
//
// Every relationship is traversable in both directions. Which entity the config
// names as `from` is an authoring detail (see
// config.SchemaRegistry.RelationshipsTouching); what direction changes is the
// CARDINALITY of the step, and that is recorded on every step because it is
// what decides whether a join multiplies rows.
//
// Immutable after New and holds no per-request state, so one Graph is safe to
// share across requests without a lock (section 22, rule 9).
package graph

import (
	"fmt"
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

// Step is one traversal of one relationship.
type Step struct {
	Relationship string
	// From and To are the join keys in TRAVERSAL order: the SQL join
	// condition is From.Entity.From.Field = To.Entity.To.Field.
	From, To config.Endpoint
	// Forward is true when the step runs in the direction the config declared.
	Forward bool
	// ToMany is true when one row on the From side can match many on the To
	// side. It is what makes "customers who ordered" a real restriction (a
	// customer may have no orders) and "orders placed by a customer" a trivial
	// one (every order has its customer).
	ToMany bool
}

// Path is a sequence of steps from one entity to another. An empty path means
// From == To.
type Path struct {
	From, To string
	Steps    []Step
}

// Len is the number of joins the path needs.
func (p Path) Len() int { return len(p.Steps) }

// Uses reports whether the path traverses the named relationship.
func (p Path) Uses(relationship string) bool {
	for _, s := range p.Steps {
		if s.Relationship == relationship {
			return true
		}
	}
	return false
}

// ToMany reports whether any step fans out. A path with no to-many step reaches
// exactly one row (or none, for a null key) for each starting row.
func (p Path) ToMany() bool {
	for _, s := range p.Steps {
		if s.ToMany {
			return true
		}
	}
	return false
}

// Entities lists the entities along the path, From first.
func (p Path) Entities() []string {
	out := []string{p.From}
	for _, s := range p.Steps {
		out = append(out, s.To.Entity)
	}
	return out
}

// String renders "customer -[customer_orders]-> order -[order_product]-> product".
// Structure only: entity and relationship names, never values.
func (p Path) String() string {
	var b strings.Builder
	b.WriteString(p.From)
	for _, s := range p.Steps {
		fmt.Fprintf(&b, " -[%s]-> %s", s.Relationship, s.To.Entity)
	}
	return b.String()
}

// Key identifies a path by its steps, for deduplication and comparison.
func (p Path) Key() string {
	var parts []string
	for _, s := range p.Steps {
		dir := ">"
		if !s.Forward {
			dir = "<"
		}
		parts = append(parts, s.Relationship+dir)
	}
	return p.From + "|" + strings.Join(parts, ",") + "|" + p.To
}

// Graph is the schema graph.
type Graph struct {
	reg *config.SchemaRegistry
	// adj holds each entity's outgoing steps, sorted by relationship name then
	// direction, so search order — and therefore every result — is fixed.
	adj      map[string][]Step
	maxJoins int
}

// New builds the graph from a validated registry.
func New(reg *config.SchemaRegistry) *Graph {
	g := &Graph{reg: reg, adj: map[string][]Step{}, maxJoins: reg.Security().MaxJoins}
	seen := map[string]bool{}
	for _, e := range reg.Entities() {
		for _, rel := range reg.RelationshipsTouching(e.Name) {
			if seen[rel.Name] {
				continue
			}
			seen[rel.Name] = true
			forwardMany := rel.Cardinality == config.OneToMany
			reverseMany := rel.Cardinality == config.ManyToOne
			g.adj[rel.From.Entity] = append(g.adj[rel.From.Entity], Step{
				Relationship: rel.Name, From: rel.From, To: rel.To, Forward: true, ToMany: forwardMany,
			})
			g.adj[rel.To.Entity] = append(g.adj[rel.To.Entity], Step{
				Relationship: rel.Name, From: rel.To, To: rel.From, Forward: false, ToMany: reverseMany,
			})
		}
	}
	for k := range g.adj {
		steps := g.adj[k]
		sort.Slice(steps, func(i, j int) bool {
			if steps[i].Relationship != steps[j].Relationship {
				return steps[i].Relationship < steps[j].Relationship
			}
			return steps[i].Forward && !steps[j].Forward
		})
	}
	return g
}

// MaxJoins is the configured join limit that bounds every search.
func (g *Graph) MaxJoins() int { return g.maxJoins }

// Neighbours returns the entities one relationship away, sorted and unique.
func (g *Graph) Neighbours(entity string) []string {
	set := map[string]bool{}
	for _, s := range g.adj[entity] {
		set[s.To.Entity] = true
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// RelatedEntity answers "field → related entity": when a field is a join key
// of some relationship, the entity on the other side. Sorted, since one key
// can take part in several relationships.
func (g *Graph) RelatedEntity(entity, field string) []string {
	set := map[string]bool{}
	for _, s := range g.adj[entity] {
		if s.From.Field == field {
			set[s.To.Entity] = true
		}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Paths returns every simple path from one entity to another within the join
// limit, in a deterministic order: shortest first, then by Key.
//
// Simple — no entity visited twice — so a self-referencing relationship is
// never traversed; a query that needs one (a manager hierarchy) is out of
// scope until a phase asks for it.
func (g *Graph) Paths(from, to string) []Path {
	if from == to {
		return []Path{{From: from, To: to}}
	}
	var out []Path
	visited := map[string]bool{from: true}
	var steps []Step
	var walk func(at string)
	walk = func(at string) {
		if len(steps) >= g.maxJoins {
			return
		}
		for _, s := range g.adj[at] {
			next := s.To.Entity
			if visited[next] {
				continue
			}
			steps = append(steps, s)
			if next == to {
				out = append(out, Path{From: from, To: to, Steps: append([]Step(nil), steps...)})
			} else {
				visited[next] = true
				walk(next)
				visited[next] = false
			}
			steps = steps[:len(steps)-1]
		}
	}
	walk(from)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Len() != out[j].Len() {
			return out[i].Len() < out[j].Len()
		}
		return out[i].Key() < out[j].Key()
	})
	return out
}

// Resolve chooses THE path from one entity to another, or refuses.
//
// The preference is fixed and explainable:
//
//  1. the path using the most relationships the sentence mentioned — "orders
//     shipped to customers" names its join, and the named one wins;
//  2. then the shortest.
//
// Two paths equal on both counts are "equally plausible and not
// disambiguated", which section 13 requires to be REJECTED — never settled by
// which relationship name sorts first. No path within the join limit is
// RELATIONSHIP_NOT_FOUND.
func (g *Graph) Resolve(from, to string, mentioned []string) (Path, error) {
	paths := g.Paths(from, to)
	if len(paths) == 0 {
		return Path{}, qferr.New(qferr.CodeRelationshipNotFound,
			"%s and %s cannot be connected: no configured relationship path within the join limit of %d",
			from, to, g.maxJoins)
	}
	score := func(p Path) int {
		n := 0
		for _, r := range mentioned {
			if p.Uses(r) {
				n++
			}
		}
		return n
	}
	best := paths[0]
	bestScore := score(best)
	for _, p := range paths[1:] {
		if s := score(p); s > bestScore || (s == bestScore && p.Len() < best.Len()) {
			best, bestScore = p, s
		}
	}
	var tied []string
	for _, p := range paths {
		if score(p) == bestScore && p.Len() == best.Len() {
			tied = append(tied, p.String())
		}
	}
	if len(tied) > 1 {
		return Path{}, qferr.New(qferr.CodeAmbiguousMatch,
			"%s and %s are connected by %d equally plausible join paths, and the sentence does not say which: %s",
			from, to, len(tied), strings.Join(tied, "; "))
	}
	return best, nil
}

// Walk follows a named sequence of relationships from an entity and returns
// the path it traces. Added in Phase 6: a SemanticPlan names its joins as
// relationship names ("via"), and the compiler must turn those names back into
// keyed, directed steps without trusting them.
//
// It refuses, rather than repairs, anything the graph would not have produced
// itself: an unknown relationship, one that does not touch the entity reached
// so far, a self-referencing one (its direction would be a guess), a path that
// revisits an entity (Paths yields simple paths only), and a path longer than
// the join limit. The refusals are SECURITY_REJECTED because the configured
// relationships are the join allowlist (section 16.6). Relationship names from
// the caller are redacted in the message: a name that is not configured is
// input, not schema.
func (g *Graph) Walk(from string, via []string) (Path, error) {
	if _, ok := g.reg.Entity(from); !ok {
		return Path{}, qferr.New(qferr.CodeSecurityRejected, "the path starts at an entity that is not configured (%s)", qferr.Sensitive(from))
	}
	if len(via) > g.maxJoins {
		return Path{}, qferr.New(qferr.CodeSecurityRejected,
			"the path from %s has %d joins, over the join limit of %d", from, len(via), g.maxJoins)
	}
	p := Path{From: from, To: from}
	visited := map[string]bool{from: true}
	at := from
	for i, name := range via {
		rel, ok := g.reg.Relationship(name)
		if !ok {
			return Path{}, qferr.New(qferr.CodeSecurityRejected,
				"step %d of the path from %s names a relationship that is not configured (%s)", i, from, qferr.Sensitive(name))
		}
		if rel.From.Entity == rel.To.Entity {
			return Path{}, qferr.New(qferr.CodeSecurityRejected,
				"step %d of the path from %s is the self-referencing relationship %s, whose direction cannot be told from its name", i, from, name)
		}
		var step Step
		found := false
		for _, s := range g.adj[at] {
			if s.Relationship == name {
				step, found = s, true
			}
		}
		if !found {
			return Path{}, qferr.New(qferr.CodeSecurityRejected,
				"step %d of the path from %s is %s, which does not touch %s", i, from, name, at)
		}
		at = step.To.Entity
		if visited[at] {
			return Path{}, qferr.New(qferr.CodeSecurityRejected,
				"the path from %s returns to %s at step %d; only simple paths are traversed", from, at, i)
		}
		visited[at] = true
		p.Steps = append(p.Steps, step)
		p.To = at
	}
	return p, nil
}

// Prefix is the path's first n steps.
func (p Path) Prefix(n int) Path {
	q := Path{From: p.From, To: p.From, Steps: p.Steps[:n:n]}
	if n > 0 {
		q.To = p.Steps[n-1].To.Entity
	}
	return q
}

// FanOut returns the index of the first to-many step, or -1 when the path
// reaches at most one row per starting row.
func (p Path) FanOut() int {
	for i, s := range p.Steps {
		if s.ToMany {
			return i
		}
	}
	return -1
}

// Names lists the relationship names along the path, the form a SemanticPlan
// carries.
func (p Path) Names() []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Relationship)
	}
	return out
}
