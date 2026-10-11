package embedding

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestDeterministicIsDeterministic(t *testing.T) {
	d := NewDeterministic(0)
	ctx := context.Background()

	first, err := d.EmbedOne(ctx, "customer spending")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := d.EmbedOne(ctx, "customer spending")
		if err != nil {
			t.Fatalf("EmbedOne: %v", err)
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("vector varies between calls at position %d", j)
			}
		}
	}

	// A second instance must agree with the first, or a persisted index would
	// stop matching queries after a restart.
	other := NewDeterministic(0)
	again, err := other.EmbedOne(ctx, "customer spending")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	if Cosine(first, again) < 0.999999 {
		t.Error("two instances disagree on the same text")
	}
}

func TestDeterministicDimensionAndNormalization(t *testing.T) {
	d := NewDeterministic(0)
	id := d.Identity()
	if id.Dim != DeterministicDim {
		t.Errorf("dim = %d, want %d", id.Dim, DeterministicDim)
	}
	if !id.Normalized {
		t.Error("identity should report normalized vectors")
	}

	// The guard that keeps a mechanism check from being mistaken for a result.
	if id.Semantic {
		t.Error("the hashing embedder must not claim to be semantic")
	}

	v, err := d.EmbedOne(context.Background(), "money spent")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	if len(v) != id.Dim {
		t.Fatalf("vector has %d dimensions, identity says %d", len(v), id.Dim)
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if math.Abs(sum-1) > 1e-5 {
		t.Errorf("vector is not unit length: |v|² = %f", sum)
	}
}

// The hashing embedder is the lexical baseline for the Phase 10 ablation, so it
// has to behave like one: surface overlap scores, meaning does not.
func TestDeterministicCapturesLexicalOverlapOnly(t *testing.T) {
	d := NewDeterministic(0)
	ctx := context.Background()

	embed := func(s string) []float32 {
		v, err := d.EmbedOne(ctx, s)
		if err != nil {
			t.Fatalf("EmbedOne(%q): %v", s, err)
		}
		return v
	}

	spending := embed("spending")
	spend := embed("customer spend")
	paraphrase := embed("money paid out by the client")

	lexical := Cosine(spending, spend)
	semantic := Cosine(spending, paraphrase)

	if lexical <= semantic {
		t.Errorf("shared characters should outscore a paraphrase: %.3f vs %.3f", lexical, semantic)
	}
	// If it scored paraphrases well it would not be a baseline worth beating.
	if semantic > 0.5 {
		t.Errorf("a pure paraphrase should score poorly, got %.3f", semantic)
	}
}

func TestDeterministicBatchMatchesSingle(t *testing.T) {
	d := NewDeterministic(0)
	ctx := context.Background()
	texts := []string{"customer", "order amount", "last year"}

	batch, err := d.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(batch) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(batch), len(texts))
	}
	for i, text := range texts {
		single, err := d.EmbedOne(ctx, text)
		if err != nil {
			t.Fatalf("EmbedOne: %v", err)
		}
		if Cosine(batch[i], single) < 0.999999 {
			t.Errorf("batch and single disagree for %q", text)
		}
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		// Unnormalised input must still score correctly: a provider reporting
		// Normalized=false would otherwise produce silently wrong scores.
		{"unnormalised but parallel", []float32{3, 4}, []float32{6, 8}, 1},
		{"mismatched lengths", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"empty", nil, nil, 0},
		{"zero vector", []float32{0, 0}, []float32{1, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cosine(tc.a, tc.b); math.Abs(got-tc.want) > 1e-6 {
				t.Errorf("Cosine = %f, want %f", got, tc.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	v := Normalize([]float32{3, 4})
	if math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Errorf("Normalize([3 4]) = %v, want [0.6 0.8]", v)
	}

	// A zero vector is left alone: dividing by zero would put NaNs into every
	// score it later touches, and NaN comparisons silently sort wrongly.
	zero := Normalize([]float32{0, 0})
	for _, x := range zero {
		if math.IsNaN(float64(x)) {
			t.Fatal("normalising a zero vector produced NaN")
		}
	}
}

func TestIdentityString(t *testing.T) {
	semantic := Identity{Model: "sentence-transformers/all-MiniLM-L6-v2", Revision: "abc123", Dim: 384, Semantic: true}
	if got := semantic.String(); !strings.Contains(got, "semantic") || !strings.Contains(got, "384") {
		t.Errorf("String = %q, want the model, dimension and semantic flag", got)
	}

	hashing := NewDeterministic(0).Identity()
	if !strings.Contains(hashing.String(), "non-semantic") {
		t.Errorf("String = %q, should mark itself non-semantic", hashing.String())
	}
}
