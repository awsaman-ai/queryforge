// Package corpus turns a validated config into the searchable semantic corpus.
//
// This is where the experiment's central bet is made concrete. Schema-linking
// research identifies vocabulary mismatch — users say "spent", the schema says
// `total_spent` — as the core difficulty, and the corpus is what closes that
// gap: it embeds the words a person would actually use, not the identifiers a
// DBA chose.
//
// Corpus generation is deterministic and needs no model. That split is
// deliberate: it means corpus structure can be tested exhaustively without
// downloading weights, and a change in retrieval results can be attributed to
// the model rather than to the text it was given.
//
// REDACTION: some surfaces are config VALUES — value aliases and enum values.
// They are legitimate working material here (embedding them is the entire
// point) but they carry the Sensitive marker so that anything rendering them
// redacts by default. Holding a value does not make it safe to print. See
// docs/decisions/006-redaction-contract.md.
package corpus

import (
	"fmt"
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

// Kind classifies what a corpus entry stands for. The set comes from section
// 4.3 of the brief.
type Kind string

const (
	KindEntity       Kind = "ENTITY"
	KindField        Kind = "FIELD"
	KindOperator     Kind = "OPERATOR"
	KindAggregation  Kind = "AGGREGATION"
	KindValueAlias   Kind = "VALUE_ALIAS"
	KindQueryPattern Kind = "QUERY_PATTERN"
	KindRelationship Kind = "RELATIONSHIP"
)

// SurfaceKind says what role a piece of text plays for its entry. It is kept
// because the pieces are not equally trustworthy evidence: an exact name match
// means more than a brushing similarity to a long description, and Phase 4's
// lexical and context scoring needs to tell them apart.
type SurfaceKind string

const (
	SurfaceName        SurfaceKind = "name"
	SurfaceDescription SurfaceKind = "description"
	SurfaceSynonym     SurfaceKind = "synonym"
	SurfacePhrase      SurfaceKind = "phrase"
	SurfaceValue       SurfaceKind = "value"
)

// Surface is one embeddable piece of text belonging to an entry.
type Surface struct {
	Kind SurfaceKind
	Text string
	// Sensitive marks text that is a config VALUE rather than schema structure.
	// Anything that renders a surface must redact these by default.
	Sensitive bool
}

// Entry is one searchable object derived from config.
//
// It deliberately holds NO embedding. The brief's sketch lists one, but keeping
// vectors out means corpus generation stays pure and testable without a model,
// and it lets the same corpus be embedded two different ways — which is exactly
// what Phase 2 sets out to compare.
type Entry struct {
	// ID is stable across runs and unique: "field:customer.total_spent".
	// Stability matters because an index is cached and reloaded, and entries
	// have to be matched back to config across process boundaries.
	ID   string
	Kind Kind

	// Entity is the owning entity, where one applies.
	Entity string
	// Ref is the specific object: field name, operator id, pattern name,
	// relationship name. Empty for entity entries, where Entity says it all.
	Ref string

	// Canonical is the entry's primary human-readable label. Structural, never
	// sensitive — it is what debug output and ambiguity messages name.
	Canonical string

	// Surfaces are every piece of text that should retrieve this entry.
	Surfaces []Surface

	Meta Meta
}

// Meta carries the structural facts Phase 4 needs for compatibility scoring.
// It is not free-form: everything here must come from validated config.
type Meta struct {
	FieldType    config.FieldType
	Operators    []config.Operator
	Aggregations []config.Aggregation
	Cardinality  config.Cardinality
	Intent       config.PatternIntent
	// Filterable and Selectable mirror the field flags, so retrieval can drop a
	// candidate that could never be used the way the sentence implies.
	Filterable bool
	Selectable bool
	// ValueTarget is the stored value a VALUE_ALIAS entry resolves to. It is a
	// config value: sensitive, never rendered by default.
	ValueTarget string
}

// Texts returns every surface's text, in order. Convenience for the per-surface
// embedding strategy.
func (e Entry) Texts() []string {
	out := make([]string, len(e.Surfaces))
	for i, s := range e.Surfaces {
		out[i] = s.Text
	}
	return out
}

// Passage returns the entry rendered as a single block of text, for the
// one-vector-per-entry strategy.
//
// The composition is deliberate. Type and entity are stated in words rather
// than as bare tokens ("A number field of customer") because the embedding
// model was trained on prose, not on schema dumps — "number" sitting alone
// contributes little, while "a number field" places it.
func (e Entry) Passage() string {
	var b strings.Builder
	b.WriteString(e.Canonical)

	for _, s := range e.Surfaces {
		switch s.Kind {
		case SurfaceName:
			// Already covered by Canonical; repeating it would weight the
			// identifier more heavily than the words users actually say.
			continue
		case SurfaceDescription:
			b.WriteString(". ")
			b.WriteString(strings.TrimSuffix(s.Text, "."))
		}
	}

	var alt []string
	for _, s := range e.Surfaces {
		if s.Kind == SurfaceSynonym || s.Kind == SurfacePhrase || s.Kind == SurfaceValue {
			alt = append(alt, s.Text)
		}
	}
	if len(alt) > 0 {
		b.WriteString(". Also known as ")
		b.WriteString(strings.Join(alt, ", "))
	}

	if e.Kind == KindField && e.Meta.FieldType != "" {
		fmt.Fprintf(&b, ". A %s field of %s", e.Meta.FieldType, e.Entity)
	}
	b.WriteString(".")
	return b.String()
}

// HasSensitiveSurface reports whether any of the entry's text is a config value.
func (e Entry) HasSensitiveSurface() bool {
	for _, s := range e.Surfaces {
		if s.Sensitive {
			return true
		}
	}
	return false
}

// Build derives the corpus from a validated config.
//
// Output is sorted by ID and therefore byte-identical across runs. That is not
// cosmetic: the index is a cached artefact keyed by a checksum of the corpus,
// and a corpus whose order drifted would invalidate its own cache on every
// startup.
func Build(r *config.SchemaRegistry) []Entry {
	var entries []Entry

	entries = append(entries, buildEntities(r)...)
	entries = append(entries, buildFields(r)...)
	entries = append(entries, buildValueEntries(r)...)
	entries = append(entries, buildOperators(r)...)
	entries = append(entries, buildAggregations(r)...)
	entries = append(entries, buildRelationships(r)...)
	entries = append(entries, buildPatterns(r)...)

	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

func buildEntities(r *config.SchemaRegistry) []Entry {
	var out []Entry
	for _, e := range r.Entities() {
		entry := Entry{
			ID:        "entity:" + e.Name,
			Kind:      KindEntity,
			Entity:    e.Name,
			Canonical: e.Name,
		}
		entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceName, Text: e.Name})
		if d := strings.TrimSpace(e.Description); d != "" {
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceDescription, Text: d})
		}
		for _, syn := range dedupe(e.Synonyms, e.Name) {
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceSynonym, Text: syn})
		}
		out = append(out, entry)
	}
	return out
}

func buildFields(r *config.SchemaRegistry) []Entry {
	var out []Entry
	for _, e := range r.Entities() {
		for _, f := range r.FieldsOf(e.Name) {
			entry := Entry{
				ID:        "field:" + f.Entity + "." + f.Name,
				Kind:      KindField,
				Entity:    f.Entity,
				Ref:       f.Name,
				Canonical: f.Entity + " " + humanize(f.Name),
				Meta: Meta{
					FieldType:    f.Type,
					Operators:    f.AllowedOperators(),
					Aggregations: f.AllowedAggregations(),
					Filterable:   f.IsFilterable(),
					Selectable:   f.IsSelectable(),
				},
			}
			// The humanised name ("total spent") is what gets embedded, not the
			// raw identifier: an embedding model has never seen `total_spent`
			// as a word, and the underscore form is exactly the vocabulary
			// mismatch the corpus exists to bridge.
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceName, Text: humanize(f.Name)})
			if d := strings.TrimSpace(f.Description); d != "" {
				entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceDescription, Text: d})
			}
			for _, syn := range dedupe(f.Synonyms, humanize(f.Name)) {
				entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceSynonym, Text: syn})
			}
			out = append(out, entry)
		}
	}
	return out
}

// buildValueEntries covers value aliases and enum values.
//
// Both are config VALUES, so every surface here is marked sensitive. They are
// embedded because they must be retrievable — "customers from India" only works
// if "India" finds the country field — but nothing may print them by default.
//
// Enum values are folded into KindValueAlias rather than given a kind of their
// own: they play the same retrieval role, and a separate kind would fragment
// the ranking rules that Phase 4 writes for values.
func buildValueEntries(r *config.SchemaRegistry) []Entry {
	var out []Entry
	for _, e := range r.Entities() {
		for _, f := range r.FieldsOf(e.Name) {
			// Sorted: both sources are maps or slices whose order must not leak
			// into the corpus, which is checksummed.
			aliases := make([]string, 0, len(f.ValueAliases))
			for alias := range f.ValueAliases {
				aliases = append(aliases, alias)
			}
			sort.Strings(aliases)

			for _, alias := range aliases {
				target := f.ValueAliases[alias]
				out = append(out, Entry{
					ID:        valueIDPrefix + f.Entity + "." + f.Name + ":alias:" + alias,
					Kind:      KindValueAlias,
					Entity:    f.Entity,
					Ref:       f.Name,
					Canonical: f.Entity + "." + f.Name + " value",
					Surfaces:  []Surface{{Kind: SurfaceValue, Text: alias, Sensitive: true}},
					Meta:      Meta{FieldType: f.Type, ValueTarget: target},
				})
			}

			values := append([]string(nil), f.EnumValues...)
			sort.Strings(values)
			for _, val := range values {
				out = append(out, Entry{
					ID:        valueIDPrefix + f.Entity + "." + f.Name + ":enum:" + val,
					Kind:      KindValueAlias,
					Entity:    f.Entity,
					Ref:       f.Name,
					Canonical: f.Entity + "." + f.Name + " value",
					Surfaces:  []Surface{{Kind: SurfaceValue, Text: val, Sensitive: true}},
					Meta:      Meta{FieldType: f.Type, ValueTarget: val},
				})
			}
		}
	}
	return out
}

func buildOperators(r *config.SchemaRegistry) []Entry {
	var out []Entry
	for _, op := range r.ConfiguredOperators() {
		entry := Entry{
			ID:        "operator:" + string(op),
			Kind:      KindOperator,
			Ref:       string(op),
			Canonical: string(op),
			Meta:      Meta{Operators: []config.Operator{op}},
		}
		for _, phrase := range r.OperatorPhrases(op) {
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfacePhrase, Text: phrase})
		}
		out = append(out, entry)
	}
	return out
}

func buildAggregations(r *config.SchemaRegistry) []Entry {
	var out []Entry
	for _, agg := range r.ConfiguredAggregations() {
		entry := Entry{
			ID:        "aggregation:" + string(agg),
			Kind:      KindAggregation,
			Ref:       string(agg),
			Canonical: string(agg),
			Meta:      Meta{Aggregations: []config.Aggregation{agg}},
		}
		for _, phrase := range r.AggregationPhrases(agg) {
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfacePhrase, Text: phrase})
		}
		out = append(out, entry)
	}
	return out
}

// buildRelationships gives traversals their own entries because a relationship
// is often the only thing a VERB in the sentence corresponds to: "customers who
// bought laptops" has no field for "bought", only an edge.
func buildRelationships(r *config.SchemaRegistry) []Entry {
	var out []Entry
	seen := map[string]bool{}
	for _, e := range r.Entities() {
		for _, rel := range r.RelationshipsTouching(e.Name) {
			if seen[rel.Name] {
				continue
			}
			seen[rel.Name] = true

			entry := Entry{
				ID:        "relationship:" + rel.Name,
				Kind:      KindRelationship,
				Entity:    rel.From.Entity,
				Ref:       rel.Name,
				Canonical: rel.From.Entity + " to " + rel.To.Entity,
				Meta:      Meta{Cardinality: rel.Cardinality},
			}
			entry.Surfaces = append(entry.Surfaces, Surface{
				Kind: SurfaceName, Text: humanize(rel.Name),
			})
			if d := strings.TrimSpace(rel.Description); d != "" {
				entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceDescription, Text: d})
			}
			for _, syn := range dedupe(rel.Synonyms, humanize(rel.Name)) {
				entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceSynonym, Text: syn})
			}
			out = append(out, entry)
		}
	}
	return out
}

func buildPatterns(r *config.SchemaRegistry) []Entry {
	var out []Entry
	for _, p := range r.QueryPatterns() {
		entry := Entry{
			ID:        "pattern:" + p.Name,
			Kind:      KindQueryPattern,
			Entity:    p.Entity,
			Ref:       p.Name,
			Canonical: p.Name,
			Meta:      Meta{Intent: p.Intent},
		}
		if d := strings.TrimSpace(p.Description); d != "" {
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfaceDescription, Text: d})
		}
		for _, phrase := range p.Phrases {
			entry.Surfaces = append(entry.Surfaces, Surface{Kind: SurfacePhrase, Text: phrase})
		}
		out = append(out, entry)
	}
	return out
}

// humanize turns a schema identifier into something an embedding model has
// actually seen: total_spent → "total spent".
func humanize(name string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(name, "_", " "), "-", " "))
}

// dedupe drops blanks and anything matching the entry's own name, so a synonym
// list that repeats the name does not get embedded twice and quietly
// double-weight it.
func dedupe(values []string, exclude string) []string {
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(exclude)): true}
	var out []string
	for _, v := range values {
		key := strings.ToLower(strings.TrimSpace(v))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

// DisplayID renders a corpus entry ID for output, applying the redaction contract.
//
// Value entries carry the config value INSIDE their ID —
// "value:customer.country:alias:India" — because the ID has to be unique per
// value and stable across runs. That makes the ID itself sensitive. Phase 2's
// --search printed it verbatim in both its text and JSON output, so the matched
// column said <redacted> while the id column beside it said "India"; found in
// Phase 4, whose ranking diagnostics print candidate ids everywhere.
//
// Structure survives: the entity, field and whether it is an alias or an enum
// value are schema shape, not stored data. Only the trailing value is withheld.
// The internal ID is unchanged — it is what the index, the fingerprint and the
// probe files key on, and redacting it there would break the cache.
func DisplayID(id string, reveal bool) string {
	prefix := valueIDPrefix
	if reveal || !strings.HasPrefix(id, prefix) {
		return id
	}
	// value:<entity>.<field>:<alias|enum>:<value>. The value is everything
	// after the third colon, so a value that itself contains colons is still
	// withheld whole rather than partly.
	parts := strings.SplitN(id, ":", 4)
	if len(parts) < 4 {
		// Not the shape Build produces. Withhold everything past the prefix
		// rather than guess where the value starts.
		return prefix + qferr.Placeholder
	}
	return parts[0] + ":" + parts[1] + ":" + parts[2] + ":" + qferr.Placeholder
}

// valueIDPrefix starts every value entry's ID. It is matched on the ID string,
// not on Kind, so that an id read from a probe file — a string with no Entry
// behind it — is redacted by the same rule.
const valueIDPrefix = "value:"
