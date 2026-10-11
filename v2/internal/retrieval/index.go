// Package retrieval holds the vector index: the corpus, its embeddings, and
// top-k search over them.
//
// It is deliberately the simplest thing that works — dense vectors in memory,
// cosine similarity, a full scan. Section 22 rule 7 says to measure before
// optimising, and an ANN structure introduced before profiling would add
// approximation error to an experiment whose whole purpose is to measure where
// the error comes from. FAISS becomes a question when Phase 9 says the scan is
// the bottleneck, not before.
package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/qferr"
)

// Strategy decides how a corpus entry becomes vectors. Phase 2 implements both
// and measures them rather than assuming; see docs/phase-2-results.md.
type Strategy string

const (
	// StrategyPassage embeds each entry as one concatenated passage: name,
	// description, synonyms and type in a single block. One vector per entry.
	StrategyPassage Strategy = "passage"

	// StrategySurface embeds every surface form separately — the name, the
	// description, each synonym — and scores an entry as the best of its own
	// vectors. More vectors, and no dilution of a rare synonym by the rest.
	StrategySurface Strategy = "surface"
)

// ValidStrategy reports whether s is a known strategy.
func ValidStrategy(s Strategy) bool {
	return s == StrategyPassage || s == StrategySurface
}

// Strategies returns every strategy, sorted.
func Strategies() []Strategy { return []Strategy{StrategyPassage, StrategySurface} }

// Index is an immutable, searchable set of embedded corpus entries.
//
// CONCURRENCY: built once at startup, read-only afterwards, safe to share
// across requests without a lock (section 22, rule 9).
type Index struct {
	identity embedding.Identity
	strategy Strategy
	// corpusHash fingerprints the corpus the vectors were built from. A cached
	// index whose hash no longer matches the config is stale and is rejected
	// rather than used — vectors for a field that has since been renamed would
	// retrieve confidently and wrongly.
	corpusHash string

	entries []corpus.Entry
	vectors [][]float32
	owners  []owner
}

// owner links a vector back to what it came from.
type owner struct {
	entry int
	// surface is the index into the entry's Surfaces, or -1 for a passage
	// vector covering the whole entry.
	surface int
}

// Match is one retrieved entry.
type Match struct {
	Entry corpus.Entry
	Score float64
	// Surface is the surface that scored best, or nil under StrategyPassage
	// where the whole entry is one vector.
	Surface *corpus.Surface
}

// MatchedText returns the text that matched, and whether it is a config value.
//
// Callers rendering this MUST redact when sensitive is true; that is the
// contract in docs/decisions/006-redaction-contract.md. A value alias is the
// single most likely thing to be retrieved by a query about a value, so this is
// not a rare path.
func (m Match) MatchedText() (text string, sensitive bool) {
	if m.Surface != nil {
		return m.Surface.Text, m.Surface.Sensitive
	}
	return m.Entry.Canonical, false
}

// Build embeds a corpus and returns a searchable index.
//
// Embedding happens in ONE batch call. That is the whole reason Provider has a
// batch method: per-text round trips to the worker would dominate build time
// for no reason.
func Build(ctx context.Context, provider embedding.Provider, entries []corpus.Entry, strategy Strategy) (*Index, error) {
	if !ValidStrategy(strategy) {
		return nil, qferr.New(qferr.CodeInvalidInput, "unknown embedding strategy %q (known: %s)",
			strategy, joinStrategies())
	}
	if len(entries) == 0 {
		return nil, qferr.New(qferr.CodeConfigError, "corpus is empty; there is nothing to search")
	}

	texts, owners := layout(entries, strategy)
	if len(texts) == 0 {
		return nil, qferr.New(qferr.CodeConfigError, "corpus produced no embeddable text")
	}

	vectors, err := provider.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, qferr.New(qferr.CodeInternal,
			"embedding provider returned %d vectors for %d texts", len(vectors), len(texts))
	}

	identity := provider.Identity()
	for i, v := range vectors {
		if len(v) != identity.Dim {
			return nil, qferr.New(qferr.CodeInternal,
				"vector %d has %d dimensions, expected %d", i, len(v), identity.Dim)
		}
	}

	return &Index{
		identity:   identity,
		strategy:   strategy,
		corpusHash: Fingerprint(entries),
		entries:    entries,
		vectors:    vectors,
		owners:     owners,
	}, nil
}

// layout produces the texts to embed and the mapping back to their entries.
func layout(entries []corpus.Entry, strategy Strategy) ([]string, []owner) {
	var texts []string
	var owners []owner

	for i, e := range entries {
		if strategy == StrategyPassage {
			texts = append(texts, e.Passage())
			owners = append(owners, owner{entry: i, surface: -1})
			continue
		}
		for j, s := range e.Surfaces {
			if strings.TrimSpace(s.Text) == "" {
				continue
			}
			texts = append(texts, s.Text)
			owners = append(owners, owner{entry: i, surface: j})
		}
	}
	return texts, owners
}

// Identity reports the model the vectors came from.
func (ix *Index) Identity() embedding.Identity { return ix.identity }

// Strategy reports how entries were embedded.
func (ix *Index) Strategy() Strategy { return ix.strategy }

// Len reports the number of corpus entries.
func (ix *Index) Len() int { return len(ix.entries) }

// VectorCount reports the number of vectors, which exceeds Len under
// StrategySurface.
func (ix *Index) VectorCount() int { return len(ix.vectors) }

// Entries returns the indexed corpus.
func (ix *Index) Entries() []corpus.Entry {
	return append([]corpus.Entry(nil), ix.entries...)
}

// Search returns the top k entries for a query vector.
//
// An entry appears at most once, scored by its best surface. Without that
// collapse, StrategySurface would fill the whole top-k with one field's five
// synonyms and hide every competing interpretation — which is precisely the
// ambiguity Phase 4 has to be able to see.
func (ix *Index) Search(query []float32, k int) ([]Match, error) {
	if len(query) != ix.identity.Dim {
		return nil, qferr.New(qferr.CodeInternal,
			"query vector has %d dimensions, index expects %d", len(query), ix.identity.Dim)
	}
	if k <= 0 {
		k = 10
	}

	best := make(map[int]Match, len(ix.entries))
	for i, vec := range ix.vectors {
		score := embedding.Cosine(query, vec)
		own := ix.owners[i]

		existing, seen := best[own.entry]
		if seen && existing.Score >= score {
			continue
		}
		entry := ix.entries[own.entry]
		match := Match{Entry: entry, Score: score}
		if own.surface >= 0 && own.surface < len(entry.Surfaces) {
			s := entry.Surfaces[own.surface]
			match.Surface = &s
		}
		best[own.entry] = match
	}

	matches := make([]Match, 0, len(best))
	for _, m := range best {
		matches = append(matches, m)
	}

	// Sorted by score, then by ID. The tie-break is not cosmetic: two entries
	// can score identically — a synonym shared between fields, an exact
	// duplicate phrase — and map iteration order would otherwise decide which
	// one a user sees as Top-1.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].Entry.ID < matches[j].Entry.ID
	})

	if len(matches) > k {
		matches = matches[:k]
	}
	return matches, nil
}

// Fingerprint hashes a corpus. Two corpora with the same fingerprint have the
// same entries, ids and texts, so an index built from one is valid for the
// other.
//
// Covers the text that was embedded AND the ids that results map back to: a
// change to either invalidates the vectors, the second more subtly than the
// first.
func Fingerprint(entries []corpus.Entry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", e.ID, e.Kind, e.Canonical)
		for _, s := range e.Surfaces {
			fmt.Fprintf(h, "%s\x1f%s\x1f", s.Kind, s.Text)
		}
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func joinStrategies() string {
	out := make([]string, 0, len(Strategies()))
	for _, s := range Strategies() {
		out = append(out, string(s))
	}
	return strings.Join(out, ", ")
}
