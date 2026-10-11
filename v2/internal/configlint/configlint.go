// Package configlint is config validation's WARNING channel (Phase 8).
//
// Validate (internal/config) rejects a config that cannot be used. This
// package reports configs that load and work but carry a phrase the engine
// will have to refuse or can never match — things an author wants to know
// about, which are not errors. Two rules, both owed by earlier gates:
//
//	phrase_shared_across_kinds  decision 019 §4: a phrase configured on
//	                            targets of incompatible semantic kinds
//	                            ("product" on an entity and a relationship)
//	phrase_never_forms          decision 016: a phrase containing words
//	                            Phase 3 always reads as a number ("one of")
//	                            or a date, which can never match as written
//
// Why a separate package: both rules need what the config package cannot
// import without a cycle — the corpus's surfaces (what retrieval actually
// matches), ranking's own definition of which readings agree and how words
// are compared, and Phase 3's own extraction. Using those rather than copies
// is the point: 016 requires the number-word check to be Phase 3's own, and
// a private copy of ranking's agreement rule would drift from what ranking
// refuses.
//
// Warnings, not errors: a shared phrase is not provably unusable — ranking
// refuses it where the sentence does not settle it, which is safe — and a
// phrase that never forms leaves its target reachable through its other
// phrases. Making either an error would also stop commerce.json ("one of",
// "product", "highest") from loading, and with it every earlier gate.
//
// REDACTION: a phrase that is a config VALUE (a value alias or enum value) is
// wrapped in qferr.Sensitive, so the default rendering withholds it
// (decision 006). Schema phrases — names, synonyms, operator and pattern
// phrases — are structure and are shown: the author needs them to find the
// line.
package configlint

import (
	"fmt"
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// phrase is one configured surface: text an author wrote that retrieval
// matches. Descriptions are excluded — they are prose, not phrases, and 019
// §5 already established that they are not evidence.
type phrase struct {
	entry     corpus.Entry
	text      string
	words     []string // ranking.FoldWords(text)
	sensitive bool
	loc       string
	kind      string // semantic kind, for the message
}

// Check returns every warning for a registry, sorted for byte-identical
// output across runs.
func Check(reg *config.SchemaRegistry) []config.Violation {
	phrases := collect(reg)
	var out []config.Violation
	out = append(out, sharedAcrossKinds(reg, phrases)...)
	out = append(out, neverForms(phrases)...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.DetailRevealed() < b.DetailRevealed()
	})
	return out
}

func collect(reg *config.SchemaRegistry) []phrase {
	loc := newLocator(reg.Config())
	var out []phrase
	for _, e := range corpus.Build(reg) {
		for _, s := range e.Surfaces {
			if s.Kind == corpus.SurfaceDescription {
				continue
			}
			out = append(out, phrase{
				entry:     e,
				text:      s.Text,
				words:     ranking.FoldWords(s.Text),
				sensitive: s.Sensitive,
				loc:       loc.of(e, s),
				kind:      kindName(e),
			})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// phrase_shared_across_kinds (019 §4)
// ---------------------------------------------------------------------------

// sharedAcrossKinds reports a phrase configured on two targets of different
// semantic kinds that ranking does not treat as one reading (ranking.Agrees).
// Two targets of the SAME kind sharing a phrase are not reported: that is the
// ordinary shared-phrase case decision 019 §5 handles at ranking time.
//
// Two shapes:
//
//   - EXACT: the same phrase on both ("product" on the product entity and the
//     order_product relationship; "highest" on max and top_n).
//   - CONTAINED: a whole phrase of one target is a word sequence inside a
//     phrase of the other ("placed" on customer_orders, inside placed_at's
//     name "placed at"). A sentence saying the shorter phrase matches the
//     first exactly and the second in part.
//
// Two exemptions, for CONTAINED only:
//
//   - The shorter phrase is a configured name or synonym of an entity that
//     OWNS the longer phrase's target — "order" in placed_at's "order date",
//     or "customer" in the join key order.customer_id's "customer id".
//     Qualifying a field's phrase with its own entity, or naming a join key
//     after the entity it references, is how configs are written. Without the
//     exemption every such phrase would warn and the real collisions would
//     drown (measured on commerce.json: 6 of 13 contained warnings were
//     join-key names, and 1 was the next case).
//   - The longer phrase is a config VALUE. A value is accepted only when the
//     sentence states it literally and in full (decision 019 §1), so a word
//     of it alone never matches the value, and the warning's claim would be
//     false.
func sharedAcrossKinds(reg *config.SchemaRegistry, phrases []phrase) []config.Violation {
	entityWords := entityPhraseWords(phrases)
	type key struct{ text, a, b string }
	seen := map[key]bool{}
	var out []config.Violation
	for i := range phrases {
		for j := range phrases {
			a, b := phrases[i], phrases[j]
			if i == j || a.entry.Kind == b.entry.Kind || a.entry.ID == b.entry.ID || ranking.Agrees(a.entry, b.entry) {
				continue
			}
			switch {
			case equalWords(a.words, b.words):
				// Each exact pair is met twice (i,j and j,i); report once,
				// from the side that sorts first.
				if a.entry.ID > b.entry.ID {
					continue
				}
				k := key{strings.Join(a.words, " "), a.entry.ID, b.entry.ID}
				if seen[k] {
					continue
				}
				seen[k] = true
				// The phrase is the same text on both sides, so it is a
				// config value if EITHER side is one: quoting it from the
				// schema side would print the value in the clear.
				shownPhrase := a
				shownPhrase.sensitive = a.sensitive || b.sensitive
				out = append(out, config.NewWarning(config.RuleSharedAcrossKinds, a.loc,
					"the phrase %s is configured on the %s %s and on the %s %s (%s); a sentence using it is refused unless its other words settle which is meant",
					quoted(shownPhrase), a.kind, display(a.entry), b.kind, display(b.entry), b.loc))
			case containsWords(b.words, a.words):
				if b.entry.Kind == corpus.KindValueAlias || ownedPhrase(reg, a.words, b.entry, entityWords) {
					continue
				}
				k := key{strings.Join(a.words, " ") + " ⊂ " + strings.Join(b.words, " "), a.entry.ID, b.entry.ID}
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, config.NewWarning(config.RuleSharedAcrossKinds, a.loc,
					"the phrase %s on the %s %s is also part of the %s %s's phrase %s (%s); a sentence using it alone matches both",
					quoted(a), a.kind, display(a.entry), b.kind, display(b.entry), quoted(b), b.loc))
			}
		}
	}
	return out
}

// entityPhraseWords maps each entity to the folded forms of its name and
// synonyms.
func entityPhraseWords(phrases []phrase) map[string][]string {
	out := map[string][]string{}
	for _, p := range phrases {
		if p.entry.Kind == corpus.KindEntity {
			out[p.entry.Entity] = append(out[p.entry.Entity], strings.Join(p.words, " "))
		}
	}
	return out
}

// ownedPhrase reports whether words is a name or synonym of an entity that
// owns target: a field's or value's entity, and for a field that is a
// relationship's join key, the entity at the relationship's other end; a
// relationship's two endpoints; an entity-scoped pattern's entity.
// Operators, aggregations and unscoped patterns have no owner.
func ownedPhrase(reg *config.SchemaRegistry, words []string, target corpus.Entry, entityWords map[string][]string) bool {
	var owners []string
	switch target.Kind {
	case corpus.KindField:
		owners = []string{target.Entity}
		key := config.Endpoint{Entity: target.Entity, Field: target.Ref}
		for _, r := range reg.Config().Relationships {
			if r.From == key {
				owners = append(owners, r.To.Entity)
			}
			if r.To == key {
				owners = append(owners, r.From.Entity)
			}
		}
	case corpus.KindValueAlias, corpus.KindEntity:
		owners = []string{target.Entity}
	case corpus.KindRelationship:
		if r, ok := reg.Relationship(target.Ref); ok {
			owners = []string{r.From.Entity, r.To.Entity}
		}
	case corpus.KindQueryPattern:
		if target.Entity != "" {
			owners = []string{target.Entity}
		}
	}
	w := strings.Join(words, " ")
	for _, o := range owners {
		for _, ew := range entityWords[o] {
			if ew == w {
				return true
			}
		}
	}
	return false
}

func equalWords(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsWords reports whether small is a contiguous, strictly shorter word
// sequence of big.
func containsWords(big, small []string) bool {
	if len(small) == 0 || len(small) >= len(big) {
		return false
	}
	for i := 0; i+len(small) <= len(big); i++ {
		if equalWords(big[i:i+len(small)], small) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// phrase_never_forms (016)
// ---------------------------------------------------------------------------

// neverForms reports a phrase in which Phase 3's own extraction finds a
// number or a date. Extraction turns those words into a number or date token
// before spans form (decisions 009, 010), so the phrase as written is never a
// text span and never matches literally. "one of" is decision 016's case; the
// date form ("last year" on a pattern) is the same defect, found by the same
// check.
//
// The check runs nlp.Analyze on the phrase itself — Phase 3's parser, not a
// list copied from it — so it cannot drift from what extraction does.
func neverForms(phrases []phrase) []config.Violation {
	var out []config.Violation
	for _, p := range phrases {
		a := nlp.Analyze(p.text, nlp.DefaultOptions)
		for _, n := range a.Numbers {
			out = append(out, config.NewWarning(config.RulePhraseNeverForms, p.loc,
				"the phrase %s on the %s %s contains %s, which phrase extraction always reads as a number, so the phrase never matches as written",
				quoted(p), p.kind, display(p.entry), wordsOf(p, a, n.Start, n.End)))
		}
		for _, d := range a.Dates {
			out = append(out, config.NewWarning(config.RulePhraseNeverForms, p.loc,
				"the phrase %s on the %s %s contains %s, which phrase extraction always reads as a date, so the phrase never matches as written",
				quoted(p), p.kind, display(p.entry), wordsOf(p, a, d.Start, d.End)))
		}
	}
	return out
}

// wordsOf renders tokens [start, end) of an analysed phrase, quoted, and
// redacted when the phrase is a config value.
func wordsOf(p phrase, a nlp.Analysis, start, end int) any {
	var ws []string
	for i := start; i < end && i < len(a.Tokens); i++ {
		ws = append(ws, a.Tokens[i].Text)
	}
	s := fmt.Sprintf("%q", strings.Join(ws, " "))
	if p.sensitive {
		return qferr.Sensitive(s)
	}
	return s
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func quoted(p phrase) any {
	if p.sensitive {
		return qferr.Sensitive(fmt.Sprintf("%q", p.text))
	}
	return fmt.Sprintf("%q", p.text)
}

// display is the entry's structural name. A value entry is named by its
// field: its ID and canonical label carry the value itself, which is
// sensitive, while the field it belongs to is schema structure.
func display(e corpus.Entry) string {
	if e.Kind == corpus.KindValueAlias {
		return e.Entity + "." + e.Ref
	}
	return e.Canonical
}

func kindName(e corpus.Entry) string {
	switch e.Kind {
	case corpus.KindEntity:
		return "entity"
	case corpus.KindField:
		return "field"
	case corpus.KindOperator:
		return "operator"
	case corpus.KindAggregation:
		return "aggregation"
	case corpus.KindValueAlias:
		return "value of field"
	case corpus.KindQueryPattern:
		return "pattern"
	case corpus.KindRelationship:
		return "relationship"
	}
	return strings.ToLower(string(e.Kind))
}

// locator renders where in the config file a phrase is written, the way an
// author searches for it: "entities[2] (product).synonyms".
type locator struct {
	entities, fields, relationships, patterns map[string]int
}

func newLocator(c *config.Config) locator {
	l := locator{entities: map[string]int{}, fields: map[string]int{}, relationships: map[string]int{}, patterns: map[string]int{}}
	for i, e := range c.Entities {
		l.entities[e.Name] = i
	}
	for i, f := range c.Fields {
		l.fields[f.Entity+"."+f.Name] = i
	}
	for i, r := range c.Relationships {
		l.relationships[r.Name] = i
	}
	for i, p := range c.QueryPatterns {
		l.patterns[p.Name] = i
	}
	return l
}

func (l locator) of(e corpus.Entry, s corpus.Surface) string {
	part := map[corpus.SurfaceKind]string{
		corpus.SurfaceName: "name", corpus.SurfaceSynonym: "synonyms", corpus.SurfacePhrase: "phrases",
	}[s.Kind]
	switch e.Kind {
	case corpus.KindEntity:
		return fmt.Sprintf("entities[%d] (%s).%s", l.entities[e.Entity], e.Entity, part)
	case corpus.KindField:
		key := e.Entity + "." + e.Ref
		return fmt.Sprintf("fields[%d] (%s).%s", l.fields[key], key, part)
	case corpus.KindValueAlias:
		key := e.Entity + "." + e.Ref
		which := "valueAliases"
		if strings.Contains(e.ID, ":enum:") {
			which = "enumValues"
		}
		return fmt.Sprintf("fields[%d] (%s).%s", l.fields[key], key, which)
	case corpus.KindRelationship:
		return fmt.Sprintf("relationships[%d] (%s).%s", l.relationships[e.Ref], e.Ref, part)
	case corpus.KindOperator:
		return "operators." + e.Ref
	case corpus.KindAggregation:
		return "aggregations." + e.Ref
	case corpus.KindQueryPattern:
		return fmt.Sprintf("queryPatterns[%d] (%s).phrases", l.patterns[e.Ref], e.Ref)
	}
	return e.ID
}
