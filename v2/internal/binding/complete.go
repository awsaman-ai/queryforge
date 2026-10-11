package binding

import (
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// This file is the Phase 8 gate's completeness rules (decision 022 §1, §2).
// Both run before any other binding rule, on the reading ranking accepted,
// and neither changes that reading: each can only refuse.
//
// They extend decision 016 ("a word nothing explains refuses the sentence")
// to the words 016 could not see. 016 looks at words OUTSIDE the accepted
// phrases. A word can also be lost INSIDE one — absorbed into a reading that
// has no use for it — or hidden as grammar. Each is the same failure: the
// query answers a broader or different question than was asked, and nothing
// in the output shows it.

// checkComplete refuses a reading that would discard part of what the
// sentence says: a number that cannot be read exactly, or a meaningful word
// the reading has no role for.
func checkComplete(in Input) error {
	if err := exactNumbers(in.Analysis); err != nil {
		return err
	}
	if err := qualifiers(in); err != nil {
		return err
	}
	return explicitTargets(in)
}

// exactNumbers refuses a number token extraction could not read exactly
// (decision 022 §2). Before the gate such a token was dropped or read as its
// leading digits: "more than 12x" compared with 12, and "customers from
// India 1.2.3" lost the token entirely.
func exactNumbers(a nlp.Analysis) error {
	for _, n := range a.Numbers {
		if n.Malformed {
			return qferr.New(qferr.CodeValueResolutionFailed,
				"the number at tokens[%d:%d] is not written in a form the engine reads exactly "+
					"(digits, optional en-US thousands grouping, one decimal point, then optionally a k, m, bn or t magnitude or a currency code; "+
					"or number words whose scale words descend, as in two million three thousand); "+
					"it is refused rather than approximated",
				n.Start, n.End)
		}
	}
	return nil
}

// qualifiers is decision 022 §1: a meaningful qualifier in a candidate phrase
// cannot be silently discarded (Phase 8 findings P10 "show product discount
// price" and P11 "show product list price", both compiled to product.price).
//
// THE PHRASE. Ranking's cover reads the sentence as spans. A phrase is a run
// of cover spans that touch (no token between them) and whose readings agree
// (the same target, or a value and its own field): "discount" and "price",
// both read as product.price, are ONE phrase. That is how the first shape of
// the finding hid: "discount" was accepted as product.price on similarity and
// entity context (0.561 + 0.15), beside "price", which names it — two spans,
// one target, and the word "discount" contributed nothing to the query.
//
// THE RULE. When a phrase contains a COMPLETE configured surface of its
// target (every word of one of its name, synonym, phrase or value surfaces),
// the target is already named in full, and the phrase must be accounted for
// by ONE such surface plus words that have another role. That is the user's
// rule at the gate, made exact:
//
//   - the complete phrase is configured for the target: its content words
//     are one configured surface's words ("product price", "unit price"), or
//   - every other word has another valid role that the reading keeps: it
//     names the entity that owns the target ("customer amount spent": the
//     root is customer either way), an endpoint of a relationship target
//     ("placed orders"), or, for a value, the value's own field ("country
//     India").
//
// Otherwise the sentence is refused (UNSUPPORTED_QUERY): the config has no
// target for the whole phrase, and reading it as the configured one would
// discard the word. A word from ANOTHER of the target's surfaces is not a
// second role: "cost price" holds the synonym "cost" and the name "price",
// but the config names no "cost price" (an accountant would say it is not the
// price at all), and "total amount" holds "total" from "order total" while
// meaning a sum. Each surface is the config author's phrase; mixing their
// words makes one the author did not write.
//
// NOT AFFECTED, deliberately:
//
//   - A phrase WITHOUT a complete surface: "money spent" for total_spent
//     ("spent" alone is none of its surfaces). That is paraphrase — the
//     embedding layer's job, and the brief's own probe — and a qualifier
//     there REPLACES a configured word ("money" for "amount") rather than
//     adding to a target already named. Where the configured word is shared
//     by several targets ("update date"), decision 021 §6 already rejects it.
//   - A word with its own reading: "average price" is two phrases (avg and
//     price), and "average" keeps its role.
//   - Separate mentions of one target that do not touch: "money spent over
//     100 sorted by total spent" is two phrases, one reading each.
//
// Similarity is not evidence here, as in decisions 019 and 021: it discovered
// "discount" as a candidate for price; only the config could say it is one of
// price's words, and it does not.
//
// REQUEST VERBS. Phase 3 marks show, list, give, get, find, display, fetch,
// return and select as function words, because they usually open a request
// ("list customers"). Inside a sentence they are content: "list price",
// "display name", "return date", "customers who return orders". A request
// verb counts as a content word unless it is the sentence's request verb (the
// first one, with only function words before it) or is followed directly by
// a function word ("give me a list of customers"). As a content word it is
// subject to the rule above when inside a phrase, and outside every phrase it
// has no role at all, so the sentence is refused, as decision 016 refuses any
// unexplained word.
func qualifiers(in Input) error {
	a, res, reg := in.Analysis, in.Ranking, in.Registry
	toks := a.Tokens
	meaningful := meaningfulVerbs(toks)
	claimConfiguredVerbs(reg, toks, meaningful)
	content := func(i int) bool {
		t := toks[i]
		return t.Kind == nlp.KindWord && (!t.Stop || meaningful[i])
	}

	// A request verb in noun position is explained only LITERALLY: by a
	// configured surface of its phrase's reading that contains the verb and is
	// complete in the phrase (a config with the synonym "list price" reads
	// "show list price"). Ranking treated the verb as a function word, so its
	// acceptance of the span says nothing about it — "return date" can be
	// accepted as order.placed_at on "date" alone. The same standard as
	// decision 021 §2 for write keywords: similarity is no explanation.
	phs := phrases(res)
	for i := range toks {
		if !meaningful[i] || verbExplained(i, toks, phs) {
			continue
		}
		return qferr.New(qferr.CodeUnsupportedQuery,
			"the word at tokens[%d:%d] is a request verb inside the sentence, not the one that opens the request: "+
				"there it qualifies what follows it or starts a second request, no configured phrase in the reading contains it, and it would be discarded",
			i, i+1)
	}

	for _, ph := range phs {
		top := ph.target
		words := ph.words(toks)
		var surfaces [][]string
		for _, sf := range top.Surfaces {
			sw := ranking.FoldWords(sf.Text)
			if configured(sf.Kind) && len(sw) > 0 && allIn(sw, words) {
				surfaces = append(surfaces, sw)
			}
		}
		if len(surfaces) == 0 {
			continue // no configured surface named in full: paraphrase, not qualification
		}
		roles := roleWords(reg, top)
		// The content words, in order, that no role explains.
		type word struct {
			at   int
			text string
		}
		var rest []word
		for i := ph.start; i < ph.end && i < len(toks); i++ {
			if !content(i) {
				continue
			}
			for _, w := range ranking.FoldWords(toks[i].Norm) {
				if !roles[w] {
					rest = append(rest, word{i, w})
				}
			}
		}
		// Accepted when one complete surface accounts for all of them. The
		// refusal names the first word outside the surface that accounts for
		// the most: the closest configured reading, and what it lacks.
		best, bestCovered := -1, -1
		for k, sw := range surfaces {
			in := set(sw)
			n := 0
			for _, w := range rest {
				if in[w.text] {
					n++
				}
			}
			if n > bestCovered {
				best, bestCovered = k, n
			}
		}
		if bestCovered == len(rest) {
			continue
		}
		in := set(surfaces[best])
		for _, w := range rest {
			if in[w.text] {
				continue
			}
			return qferr.New(qferr.CodeUnsupportedQuery,
				"the word at tokens[%d:%d] qualifies the phrase at tokens[%d:%d], which reads as %s by one of its configured names, "+
					"but the phrase as a whole is none of %s's configured names, synonyms or phrases, and the word has no other role in the reading: "+
					"the config has no target for the whole phrase, and reading it as %s would discard the word",
				w.at, w.at+1, ph.start, ph.end, display(top), display(top), display(top))
		}
	}
	return nil
}

func set(words []string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

func allIn(words []string, have map[string]bool) bool {
	for _, w := range words {
		if !have[w] {
			return false
		}
	}
	return true
}

// explicitTargets is decision 022 §5, the rule the user approved at the gate
// for findings G33 and G34:
//
//	products with price and discount above 10   →  price > 10
//	price of discount                           →  SELECT price
//
// In both, "discount" was accepted as product.price on similarity alone, while
// the sentence names product.price by its own configured word. §1 does not see
// it, because the two readings are not one phrase: a connective or a function
// word separates them, so neither qualifies the other. The word is discarded
// all the same — the reading it produces is one the sentence already has.
//
// THE RULE, in the user's words: if the sentence already explicitly names a
// configured target, another INDEPENDENT span must not resolve to that same
// target by similarity alone.
//
//   - EXPLICIT means the config's own words: a phrase holding a content word
//     of one of its target's configured names, synonyms, phrases or values.
//     This is the line decisions 019, 021 §2 and 022 §1 all draw; a
//     description is prose and never counts.
//   - BY SIMILARITY ALONE means a phrase with no such word: retrieval and
//     context proposed the target, and nothing the sentence wrote names it.
//   - THE SAME TARGET means the same corpus entry. A value and its own field
//     are NOT the same target: "clients located in India" reads "located" as
//     customer.country by similarity while "India" is one of its values, and
//     that sentence is a correct Phase 7 answer (V03). The wider form, which
//     counted them as one, refused it — measured, and rejected.
//
// Otherwise the sentence is refused: UNSUPPORTED_QUERY, naming both spans.
//
// PARAPHRASE IS UNTOUCHED, which is the point of the "alone": a span with
// independent configured evidence keeps its reading however often the target
// is named. "customers with money spent over 100 sorted by total spent" holds
// "spent", a configured word of total_spent, in both places. And a paraphrase
// of a target the sentence does NOT otherwise name is what the embedding layer
// is for: "clients located in India", "customers registered last year".
func explicitTargets(in Input) error {
	toks := in.Analysis.Tokens
	meaningful := meaningfulVerbs(toks)
	claimConfiguredVerbs(in.Registry, toks, meaningful)
	content := func(i int) bool {
		t := toks[i]
		return t.Kind == nlp.KindWord && (!t.Stop || meaningful[i])
	}
	phs := phrases(in.Ranking)

	// Where each target is named by the config's own words. First mention
	// wins, so the message names the same span whatever the sentence's order.
	named := map[string]phrase{}
	for _, ph := range phs {
		if _, seen := named[ph.target.ID]; !seen && namesItsTarget(ph, toks, content) {
			named[ph.target.ID] = ph
		}
	}
	for _, ph := range phs {
		if namesItsTarget(ph, toks, content) {
			continue
		}
		explicit, ok := named[ph.target.ID]
		if !ok {
			continue
		}
		return qferr.New(qferr.CodeUnsupportedQuery,
			"the phrase at tokens[%d:%d] resolves to %s by similarity alone, and the sentence already names %s at tokens[%d:%d] "+
				"by one of its configured names, synonyms or phrases: an independent phrase may not resolve to a target the sentence "+
				"already names, because its own words then say nothing the reading keeps",
			ph.start, ph.end, display(ph.target), display(ph.target), explicit.start, explicit.end)
	}
	return nil
}

// namesItsTarget reports whether the phrase holds a content word that is one
// of its target's configured words: the config's own vocabulary, not the
// model's judgement.
func namesItsTarget(ph phrase, toks []nlp.Token, content func(int) bool) bool {
	own := map[string]bool{}
	for _, sf := range ph.target.Surfaces {
		if !configured(sf.Kind) {
			continue
		}
		for _, w := range ranking.FoldWords(sf.Text) {
			own[w] = true
		}
	}
	for i := ph.start; i < ph.end && i < len(toks); i++ {
		if !content(i) {
			continue
		}
		for _, w := range ranking.FoldWords(toks[i].Norm) {
			if own[w] {
				return true
			}
		}
	}
	return false
}

// verbExplained reports whether the token at i lies in a phrase whose target
// has a configured surface that contains the token's word and is complete in
// the phrase.
func verbExplained(i int, toks []nlp.Token, phs []phrase) bool {
	for _, ph := range phs {
		if i < ph.start || i >= ph.end {
			continue
		}
		words := ph.words(toks)
		verb := ranking.FoldWords(toks[i].Norm)
		for _, sf := range ph.target.Surfaces {
			sw := ranking.FoldWords(sf.Text)
			if configured(sf.Kind) && len(sw) > 0 && allIn(sw, words) && allIn(verb, set(sw)) {
				return true
			}
		}
		return false
	}
	return false
}

// phrase is a run of touching cover spans whose readings agree.
type phrase struct {
	start, end int
	target     corpus.Entry
}

func (p phrase) words(toks []nlp.Token) map[string]bool {
	out := map[string]bool{}
	for i := p.start; i < p.end && i < len(toks); i++ {
		for _, w := range ranking.FoldWords(toks[i].Norm) {
			out[w] = true
		}
	}
	return out
}

// phrases groups the cover, left to right. The cover is non-overlapping and
// sorted by position (ranking.chooseCover), and binding only runs on a
// reading whose covered spans were all accepted.
func phrases(res ranking.Result) []phrase {
	var out []phrase
	for _, c := range res.Cover {
		s := res.Spans[c]
		top, ok := s.TopCandidate()
		if !ok {
			continue
		}
		if n := len(out); n > 0 && out[n-1].end == s.Span.Start && ranking.Agrees(out[n-1].target, top.Entry) {
			out[n-1].end = s.Span.End
			// The phrase keeps the reading with the more specific entry: a
			// field rather than one of its values would lose the value's
			// surfaces, so a value wins; otherwise the first reading stays.
			if top.Entry.Kind == corpus.KindValueAlias {
				out[n-1].target = top.Entry
			}
			continue
		}
		out = append(out, phrase{start: s.Span.Start, end: s.Span.End, target: top.Entry})
	}
	return out
}

// meaningfulVerbs marks the request verbs that are content words by position
// (see qualifiers). The sentence's request verb is the first request verb
// with nothing but function words and punctuation before it.
func meaningfulVerbs(toks []nlp.Token) []bool {
	lead := -1
	for i, t := range toks {
		if t.Kind == nlp.KindWord && nlp.IsRequestVerb(t.Norm) {
			lead = i
			break
		}
		if t.Kind == nlp.KindNumber || (t.Kind == nlp.KindWord && !t.Stop) {
			break // content first: no request position
		}
	}
	out := make([]bool, len(toks))
	for i, t := range toks {
		if t.Kind != nlp.KindWord || !nlp.IsRequestVerb(t.Norm) || i == lead {
			continue
		}
		if i+1 < len(toks) && toks[i+1].Kind == nlp.KindWord && toks[i+1].Stop {
			continue // "a list of customers": a framing noun
		}
		out[i] = true
	}
	return out
}

// claimConfiguredVerbs is decision 022 §6, from the gate's investigation of
// `list price of products`.
//
// A request verb that OPENS the request is grammar, not content
// (meaningfulVerbs): "list customers from India" asks for customers. That is
// how "list price of products" reads as the command "list the price of
// products" and compiles to product.price — which is right when the config
// has no list price, and wrong when it has one.
//
// So the CONFIG decides, not the position: when a configured surface contains
// the verb's word and every word of that surface is in the sentence, the
// sentence spells out a target whose own name holds the verb, and the verb may
// not be swallowed as the request frame. It becomes content, and the rule
// above then requires the reading to name that target literally — otherwise
// the sentence is refused rather than answered from a reading that dropped
// the word.
//
// Measured: with "list price" configured, every phrasing tried already read
// the configured field (18 sentences, two configs), so this rule changed no
// measured outcome. It makes that a guarantee instead of a coincidence of
// ranking, in the way decisions 021 §2 and 022 §1 make literal evidence the
// standard. With a config that has no such surface — commerce — nothing
// changes: "list customers from India" and "list price of products" both read
// the verb as the request.
//
// The surface must run from the verb, word for word: a noun compound is
// contiguous ("list price"), as §1's phrases are. The looser test — every word
// of the surface somewhere in the sentence, as decision 019 §5 uses for
// choosing between targets — refused `list products by price`, an ordinary
// command, because "list" and "price" both appear in it.
func claimConfiguredVerbs(reg *config.SchemaRegistry, toks []nlp.Token, meaningful []bool) {
	lead := -1
	for i := range toks {
		if meaningful[i] {
			continue
		}
		if toks[i].Kind == nlp.KindWord && nlp.IsRequestVerb(toks[i].Norm) {
			lead = i
			break
		}
	}
	if lead < 0 {
		return
	}
	// The words from the verb on, in order: a compound is contiguous.
	var run []string
	for i := lead; i < len(toks); i++ {
		if toks[i].Kind != nlp.KindWord {
			break
		}
		run = append(run, ranking.FoldWords(toks[i].Norm)...)
	}
	for _, surface := range configuredSurfaces(reg) {
		sw := ranking.FoldWords(surface)
		if len(sw) < 2 || len(sw) > len(run) {
			continue
		}
		same := true
		for k, w := range sw {
			if run[k] != w {
				same = false
				break
			}
		}
		if same {
			meaningful[lead] = true
			return
		}
	}
}

// configuredSurfaces is every phrase the config gives a target: entity and
// field names and synonyms, value aliases and enum values, operator,
// aggregation and pattern phrases, relationship synonyms. Descriptions are
// prose and never count (decision 019).
func configuredSurfaces(reg *config.SchemaRegistry) []string {
	cfg := reg.Config()
	var out []string
	for _, e := range cfg.Entities {
		out = append(out, e.Name)
		out = append(out, e.Synonyms...)
	}
	for _, f := range cfg.Fields {
		out = append(out, f.Name)
		out = append(out, f.Synonyms...)
		out = append(out, f.EnumValues...)
		for alias := range f.ValueAliases {
			out = append(out, alias)
		}
	}
	for _, r := range cfg.Relationships {
		out = append(out, r.Name)
		out = append(out, r.Synonyms...)
	}
	for _, phrases := range cfg.OperatorPhrases {
		out = append(out, phrases...)
	}
	for _, phrases := range cfg.AggregationPhrases {
		out = append(out, phrases...)
	}
	for _, p := range cfg.QueryPatterns {
		out = append(out, p.Name)
		out = append(out, p.Phrases...)
	}
	return out
}

// configuredKinds are the surfaces that are the config's own words for a
// target. Descriptions are prose and never count (decision 019).
func configured(k corpus.SurfaceKind) bool {
	switch k {
	case corpus.SurfaceName, corpus.SurfaceSynonym, corpus.SurfacePhrase, corpus.SurfaceValue:
		return true
	}
	return false
}

// roleWords are the words that name something the reading of e keeps without
// saying it: the entity that owns a field or value, a relationship's two
// entities, a value's own field. Naming one of them changes nothing in the
// binding, so the word is not discarded.
func roleWords(reg *config.SchemaRegistry, e corpus.Entry) map[string]bool {
	out := map[string]bool{}
	addEntity := func(name string) {
		ent, ok := reg.Entity(name)
		if !ok {
			return
		}
		for _, w := range ranking.FoldWords(ent.Name) {
			out[w] = true
		}
		for _, syn := range ent.Synonyms {
			for _, w := range ranking.FoldWords(syn) {
				out[w] = true
			}
		}
	}
	switch e.Kind {
	case corpus.KindEntity, corpus.KindField:
		addEntity(e.Entity)
	case corpus.KindValueAlias:
		addEntity(e.Entity)
		if f, ok := reg.Field(e.Entity, e.Ref); ok {
			for _, w := range ranking.FoldWords(f.Name) {
				out[w] = true
			}
			for _, syn := range f.Synonyms {
				for _, w := range ranking.FoldWords(syn) {
					out[w] = true
				}
			}
		}
	case corpus.KindRelationship:
		if rel, ok := reg.Relationship(e.Ref); ok {
			addEntity(rel.From.Entity)
			addEntity(rel.To.Entity)
		}
	}
	return out
}

// display names a target in a message: structure always, a value's value
// never (decision 006).
func display(e corpus.Entry) string { return corpus.DisplayID(e.ID, false) }
