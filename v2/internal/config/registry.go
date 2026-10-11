package config

import (
	"sort"
	"strings"
)

// SchemaRegistry is an indexed, read-only view of a validated config.
//
// It exists so that later phases never scan slices to answer "does this entity
// have that field?" — that question is asked once per candidate per span, and
// Phase 9 has a warm p50 budget of 50 ms for the whole request.
//
// CONCURRENCY: a registry is built once at startup and is immutable afterwards,
// which is what makes it safe to share across concurrent requests without a
// lock (section 22, rule 9). Every accessor that returns a slice returns a fresh
// copy, so a caller cannot reach into the index and change it.
type SchemaRegistry struct {
	cfg *Config

	entities map[string]*Entity
	fields   map[fieldKey]*Field
	// byEntity is pre-sorted by field name so that iteration order is stable;
	// anything derived from it — corpus order, candidate order, debug output —
	// inherits that determinism for free.
	byEntity map[string][]*Field

	relationships map[string]*Relationship
	// fromEntity indexes relationships by their originating entity, the
	// direction Phase 5's traversal walks.
	fromEntity map[string][]*Relationship
	// touching indexes by BOTH endpoints. A join is usable in either direction,
	// and "customers who bought laptops" starts from whichever entity the
	// sentence named first, not whichever side the config declared as "from".
	touching map[string][]*Relationship

	patterns []*QueryPattern
}

// NewSchemaRegistry indexes a config. The config must already be valid: the
// registry assumes referential integrity that Validate is what establishes.
func NewSchemaRegistry(cfg *Config) *SchemaRegistry {
	r := &SchemaRegistry{
		cfg:           cfg,
		entities:      make(map[string]*Entity, len(cfg.Entities)),
		fields:        make(map[fieldKey]*Field, len(cfg.Fields)),
		byEntity:      make(map[string][]*Field, len(cfg.Entities)),
		relationships: make(map[string]*Relationship, len(cfg.Relationships)),
		fromEntity:    make(map[string][]*Relationship, len(cfg.Entities)),
		touching:      make(map[string][]*Relationship, len(cfg.Entities)),
	}

	for i := range cfg.Entities {
		e := &cfg.Entities[i]
		r.entities[e.Name] = e
	}
	for i := range cfg.Fields {
		f := &cfg.Fields[i]
		r.fields[fieldKey{f.Entity, f.Name}] = f
		r.byEntity[f.Entity] = append(r.byEntity[f.Entity], f)
	}
	for _, fields := range r.byEntity {
		sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	}

	for i := range cfg.Relationships {
		rel := &cfg.Relationships[i]
		r.relationships[rel.Name] = rel
		r.fromEntity[rel.From.Entity] = append(r.fromEntity[rel.From.Entity], rel)
		r.touching[rel.From.Entity] = append(r.touching[rel.From.Entity], rel)
		// A self-referencing relationship (manager → employee on one entity)
		// must not be indexed twice under the same key.
		if rel.To.Entity != rel.From.Entity {
			r.touching[rel.To.Entity] = append(r.touching[rel.To.Entity], rel)
		}
	}
	for _, rels := range r.fromEntity {
		sort.Slice(rels, func(i, j int) bool { return rels[i].Name < rels[j].Name })
	}
	for _, rels := range r.touching {
		sort.Slice(rels, func(i, j int) bool { return rels[i].Name < rels[j].Name })
	}

	for i := range cfg.QueryPatterns {
		r.patterns = append(r.patterns, &cfg.QueryPatterns[i])
	}
	sort.Slice(r.patterns, func(i, j int) bool { return r.patterns[i].Name < r.patterns[j].Name })

	return r
}

// Config returns the config the registry was built from.
func (r *SchemaRegistry) Config() *Config { return r.cfg }

// Entity looks up an entity by exact name. Semantic matching is the retrieval
// layer's job; this is the exact-name index it resolves candidates against.
func (r *SchemaRegistry) Entity(name string) (*Entity, bool) {
	e, ok := r.entities[name]
	return e, ok
}

// Field looks up a field by entity and field name.
func (r *SchemaRegistry) Field(entity, field string) (*Field, bool) {
	f, ok := r.fields[fieldKey{entity, field}]
	return f, ok
}

// Entities returns every entity, sorted by name.
func (r *SchemaRegistry) Entities() []*Entity {
	out := make([]*Entity, 0, len(r.entities))
	for i := range r.cfg.Entities {
		out = append(out, &r.cfg.Entities[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FieldsOf returns an entity's fields, sorted by name.
func (r *SchemaRegistry) FieldsOf(entity string) []*Field {
	return append([]*Field(nil), r.byEntity[entity]...)
}

// Relationship looks up a relationship by name.
func (r *SchemaRegistry) Relationship(name string) (*Relationship, bool) {
	rel, ok := r.relationships[name]
	return rel, ok
}

// RelationshipsFrom returns relationships declared as originating at entity.
func (r *SchemaRegistry) RelationshipsFrom(entity string) []*Relationship {
	return append([]*Relationship(nil), r.fromEntity[entity]...)
}

// RelationshipsTouching returns relationships with entity on either side.
//
// This, not RelationshipsFrom, is what join-path search should walk: "which
// entity is the from side" is a config-authoring detail, and a traversal that
// respected it would fail to connect entities that are in fact connected.
func (r *SchemaRegistry) RelationshipsTouching(entity string) []*Relationship {
	return append([]*Relationship(nil), r.touching[entity]...)
}

// QueryPatterns returns every query pattern, sorted by name.
func (r *SchemaRegistry) QueryPatterns() []*QueryPattern {
	return append([]*QueryPattern(nil), r.patterns...)
}

// Security returns the effective complexity limits.
func (r *SchemaRegistry) Security() SecurityPolicy { return r.cfg.Security }

// OperatorPhrases returns the phrases configured for op, sorted.
func (r *SchemaRegistry) OperatorPhrases(op Operator) []string {
	out := append([]string(nil), r.cfg.OperatorPhrases[op]...)
	sort.Strings(out)
	return out
}

// ConfiguredOperators returns the operators that have phrases, sorted.
//
// An operator with no phrases is unreachable from natural language, so this —
// not the full vocabulary — is the set the semantic corpus should be built from.
func (r *SchemaRegistry) ConfiguredOperators() []Operator {
	out := make([]Operator, 0, len(r.cfg.OperatorPhrases))
	for op := range r.cfg.OperatorPhrases {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AggregationPhrases returns the phrases configured for agg, sorted.
func (r *SchemaRegistry) AggregationPhrases(agg Aggregation) []string {
	out := append([]string(nil), r.cfg.AggregationPhrases[agg]...)
	sort.Strings(out)
	return out
}

// ConfiguredAggregations returns the aggregations that have phrases, sorted.
func (r *SchemaRegistry) ConfiguredAggregations() []Aggregation {
	out := make([]Aggregation, 0, len(r.cfg.AggregationPhrases))
	for agg := range r.cfg.AggregationPhrases {
		out = append(out, agg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ResolveValueAlias maps a natural-language value to the stored value for a
// field, case-insensitively. The second result reports whether an alias matched.
//
// Case-insensitive because users type "india", not "India". The validator
// rejects aliases that differ only by case, so this cannot silently pick one of
// two plausible targets.
func (r *SchemaRegistry) ResolveValueAlias(entity, field, value string) (string, bool) {
	f, ok := r.Field(entity, field)
	if !ok {
		return "", false
	}
	for alias, target := range f.ValueAliases {
		if strings.EqualFold(strings.TrimSpace(alias), strings.TrimSpace(value)) {
			return target, true
		}
	}
	return "", false
}
