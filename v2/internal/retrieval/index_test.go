package retrieval

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
)

// Every test here uses the deterministic hashing provider. These tests are
// about index MECHANICS — build, search, persist, reload, reject — none of
// which need meaning. Semantic quality is measured separately, against the real
// model, and recorded in docs/phase-2-results.md.

func testCorpus(t *testing.T, name string) []corpus.Entry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s rejected: %v", name, err)
	}
	return corpus.Build(config.NewSchemaRegistry(cfg))
}

func buildIndex(t *testing.T, name string, strategy Strategy) (*Index, embedding.Provider) {
	t.Helper()
	provider := embedding.NewDeterministic(0)
	ix, err := Build(context.Background(), provider, testCorpus(t, name), strategy)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ix, provider
}

func TestBuildBothStrategies(t *testing.T) {
	entries := testCorpus(t, "commerce.json")

	passage, _ := buildIndex(t, "commerce.json", StrategyPassage)
	if passage.Len() != len(entries) {
		t.Errorf("passage index has %d entries, want %d", passage.Len(), len(entries))
	}
	if passage.VectorCount() != len(entries) {
		t.Errorf("passage strategy should produce one vector per entry, got %d for %d",
			passage.VectorCount(), len(entries))
	}

	surface, _ := buildIndex(t, "commerce.json", StrategySurface)
	if surface.Len() != len(entries) {
		t.Errorf("surface index has %d entries, want %d", surface.Len(), len(entries))
	}
	// One vector per surface form, so strictly more than one per entry for a
	// config with descriptions and synonyms.
	if surface.VectorCount() <= len(entries) {
		t.Errorf("surface strategy should produce more vectors (%d) than entries (%d)",
			surface.VectorCount(), len(entries))
	}
}

func TestBuildRejectsUnknownStrategy(t *testing.T) {
	_, err := Build(context.Background(), embedding.NewDeterministic(0), testCorpus(t, "commerce.json"), "nearest")
	if err == nil {
		t.Fatal("an unknown strategy was accepted")
	}
	if !strings.Contains(err.Error(), "unknown embedding strategy") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

func TestBuildRejectsEmptyCorpus(t *testing.T) {
	if _, err := Build(context.Background(), embedding.NewDeterministic(0), nil, StrategySurface); err == nil {
		t.Fatal("an empty corpus was accepted")
	}
}

// The brief asks for embedding dimension validation. A provider whose vectors
// do not match its declared width would misalign every vector with the wrong
// entry — retrieval would still "work" and be wrong everywhere.
func TestBuildRejectsWrongDimension(t *testing.T) {
	_, err := Build(context.Background(), &liarProvider{dim: 8, actual: 4}, testCorpus(t, "customers.json"), StrategyPassage)
	if err == nil {
		t.Fatal("a provider returning short vectors was accepted")
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error = %v, should name the dimension mismatch", err)
	}
}

func TestBuildRejectsWrongVectorCount(t *testing.T) {
	_, err := Build(context.Background(), &shortProvider{dim: 8}, testCorpus(t, "customers.json"), StrategyPassage)
	if err == nil {
		t.Fatal("a provider returning too few vectors was accepted")
	}
	if !strings.Contains(err.Error(), "vectors for") {
		t.Errorf("error = %v, should name the count mismatch", err)
	}
}

func TestSearchRejectsWrongQueryDimension(t *testing.T) {
	ix, _ := buildIndex(t, "customers.json", StrategyPassage)
	if _, err := ix.Search([]float32{1, 2, 3}, 5); err == nil {
		t.Fatal("a query of the wrong width was accepted")
	}
}

// Top-k retrieval determinism, from the brief's test list. Ties are the hazard:
// two entries can score identically, and map iteration order would otherwise
// decide which one a user sees as Top-1.
func TestSearchIsDeterministic(t *testing.T) {
	ix, provider := buildIndex(t, "commerce.json", StrategySurface)
	q, err := provider.EmbedOne(context.Background(), "customer spending")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}

	first, err := ix.Search(q, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("search returned nothing")
	}

	for i := 0; i < 25; i++ {
		again, err := ix.Search(q, 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(again) != len(first) {
			t.Fatalf("result count varies: %d then %d", len(first), len(again))
		}
		for j := range again {
			if again[j].Entry.ID != first[j].Entry.ID || again[j].Score != first[j].Score {
				t.Fatalf("result %d varies: %s/%f then %s/%f",
					j, first[j].Entry.ID, first[j].Score, again[j].Entry.ID, again[j].Score)
			}
		}
	}
}

// Identical scores must break by entry ID, not by whatever the map yielded.
func TestTiesBreakDeterministically(t *testing.T) {
	// A provider returning the same vector for everything makes every score
	// identical, so the ordering is decided purely by the tie-break.
	ix, err := Build(context.Background(), &constantProvider{dim: 16}, testCorpus(t, "commerce.json"), StrategyPassage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	q := make([]float32, 16)
	q[0] = 1

	first, err := ix.Search(q, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Entry.ID > first[i].Entry.ID {
			t.Fatalf("tied results are not ordered by id: %q before %q",
				first[i-1].Entry.ID, first[i].Entry.ID)
		}
	}
	for i := 0; i < 15; i++ {
		again, _ := ix.Search(q, 20)
		for j := range again {
			if again[j].Entry.ID != first[j].Entry.ID {
				t.Fatalf("tied ordering varies between calls at %d", j)
			}
		}
	}
}

// An entry must appear once, scored by its best surface. Without the collapse,
// one field's five synonyms would fill the whole top-k and hide every competing
// interpretation — exactly the ambiguity Phase 4 has to be able to see.
func TestSearchCollapsesAnEntryToItsBestSurface(t *testing.T) {
	ix, provider := buildIndex(t, "commerce.json", StrategySurface)
	q, err := provider.EmbedOne(context.Background(), "spending")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	matches, err := ix.Search(q, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	seen := map[string]bool{}
	for _, m := range matches {
		if seen[m.Entry.ID] {
			t.Errorf("entry %s appears more than once", m.Entry.ID)
		}
		seen[m.Entry.ID] = true
	}
}

func TestSearchRespectsK(t *testing.T) {
	ix, provider := buildIndex(t, "commerce.json", StrategySurface)
	q, _ := provider.EmbedOne(context.Background(), "customer")

	for _, k := range []int{1, 3, 5, 10} {
		matches, err := ix.Search(q, k)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(matches) > k {
			t.Errorf("k=%d returned %d matches", k, len(matches))
		}
	}

	// Results must be ordered by score, descending.
	matches, _ := ix.Search(q, 20)
	for i := 1; i < len(matches); i++ {
		if matches[i-1].Score < matches[i].Score {
			t.Errorf("results are not sorted by score at position %d", i)
		}
	}
}

// MatchedText is what a caller renders, so it must report the sensitivity of
// what it hands back — a value alias is the most likely thing to be retrieved
// by a query about a value.
func TestMatchedTextReportsSensitivity(t *testing.T) {
	ix, provider := buildIndex(t, "commerce.json", StrategySurface)
	q, _ := provider.EmbedOne(context.Background(), "laptops")

	matches, err := ix.Search(q, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var sawValue bool
	for _, m := range matches {
		text, sensitive := m.MatchedText()
		if m.Entry.Kind == corpus.KindValueAlias {
			sawValue = true
			if !sensitive {
				t.Errorf("value entry %s reported its text %q as safe", m.Entry.ID, text)
			}
			continue
		}
		if sensitive {
			t.Errorf("entry %s (%s) should not be sensitive", m.Entry.ID, m.Entry.Kind)
		}
	}
	if !sawValue {
		t.Skip("no value entry in the top 20 for this query under the hashing embedder")
	}
}

func TestFingerprintChangesWithCorpus(t *testing.T) {
	a := testCorpus(t, "commerce.json")
	b := testCorpus(t, "commerce.json")
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("the same corpus produced different fingerprints")
	}
	if Fingerprint(a) == Fingerprint(testCorpus(t, "customers.json")) {
		t.Fatal("different corpora produced the same fingerprint")
	}

	// A renamed field must invalidate the index. The id is what results map
	// back through, so a stale index would answer under the old name.
	modified := append([]corpus.Entry(nil), a...)
	modified[0].ID = modified[0].ID + "_renamed"
	if Fingerprint(modified) == Fingerprint(a) {
		t.Error("changing an entry id should change the fingerprint")
	}

	modified = append([]corpus.Entry(nil), a...)
	surfaces := append([]corpus.Surface(nil), modified[0].Surfaces...)
	surfaces[0].Text = "something else entirely"
	modified[0].Surfaces = surfaces
	if Fingerprint(modified) == Fingerprint(a) {
		t.Error("changing surface text should change the fingerprint")
	}
}

// --- providers that misbehave on purpose ---

type liarProvider struct{ dim, actual int }

func (p *liarProvider) Identity() embedding.Identity {
	return embedding.Identity{Model: "liar", Dim: p.dim}
}
func (p *liarProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, p.actual)
	}
	return out, nil
}
func (p *liarProvider) EmbedOne(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, p.actual), nil
}
func (p *liarProvider) Close() error { return nil }

type shortProvider struct{ dim int }

func (p *shortProvider) Identity() embedding.Identity {
	return embedding.Identity{Model: "short", Dim: p.dim}
}
func (p *shortProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts)-1)
	for i := range out {
		out[i] = make([]float32, p.dim)
	}
	return out, nil
}
func (p *shortProvider) EmbedOne(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, p.dim), nil
}
func (p *shortProvider) Close() error { return nil }

type constantProvider struct{ dim int }

func (p *constantProvider) Identity() embedding.Identity {
	return embedding.Identity{Model: "constant", Dim: p.dim, Normalized: true}
}
func (p *constantProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, p.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}
func (p *constantProvider) EmbedOne(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, p.dim)
	v[0] = 1
	return v, nil
}
func (p *constantProvider) Close() error { return nil }
