package embedding

import (
	"context"
	"hash/fnv"
	"strings"
)

// Deterministic is a character-trigram hashing embedder. It needs no model, no
// Python and no network, and the same text always yields the same vector.
//
// It exists for two reasons.
//
// First, tests. The corpus builder, the index, persistence and the retrieval
// mechanics all need vectors, but none of them need MEANING. Making those tests
// depend on an 80 MB download would make them slow, flaky offline, and
// unrunnable in a fresh checkout — and a test that is hard to run stops being
// run.
//
// Second, it is an honest lexical baseline. Trigram hashing captures surface
// overlap and nothing else: "spending" and "spend" score well together,
// "spending" and "money paid out" do not. That is exactly ablation A in section
// 18 — the lexical/rule baseline that the embedding results have to beat before
// anyone can claim vectors are earning their place.
//
// Identity reports Semantic=false, and the CLI refuses to record research
// results from a provider that says so. A number produced here is a mechanism
// check, never a finding.
type Deterministic struct {
	dim int
}

// DeterministicDim is the vector width. Smaller than a real model's 384 because
// there is no reason for it to be larger: trigram hashing saturates long before
// that, and a smaller vector keeps the test suite quick.
const DeterministicDim = 256

// NewDeterministic returns a hashing embedder. A dim of zero uses
// DeterministicDim.
func NewDeterministic(dim int) *Deterministic {
	if dim <= 0 {
		dim = DeterministicDim
	}
	return &Deterministic{dim: dim}
}

func (d *Deterministic) Identity() Identity {
	return Identity{
		Model:      "deterministic-trigram-hash",
		Revision:   "v1",
		Dim:        d.dim,
		Normalized: true,
		Semantic:   false,
	}
}

func (d *Deterministic) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i] = d.vector(text)
	}
	return out, nil
}

func (d *Deterministic) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.vector(text), nil
}

// Close is a no-op: there is nothing to release.
func (d *Deterministic) Close() error { return nil }

// vector hashes the text's character trigrams into a fixed-width vector.
//
// Lowercased and space-padded so that word boundaries participate: padding
// "spend" to " spend " means the leading and trailing trigrams encode that it
// is a whole word, which is what separates "spend" from "backspending".
func (d *Deterministic) vector(text string) []float32 {
	v := make([]float32, d.dim)
	normalized := " " + strings.Join(strings.Fields(strings.ToLower(text)), " ") + " "

	runes := []rune(normalized)
	for i := 0; i+3 <= len(runes); i++ {
		h := fnv.New32a()
		// Errors from a hash Writer are documented never to occur.
		_, _ = h.Write([]byte(string(runes[i : i+3])))
		sum := h.Sum32()

		// The low bit picks a sign, so unrelated trigrams cancel rather than
		// always accumulating — without it every vector drifts toward the same
		// all-positive corner and everything looks similar to everything.
		bucket := int(sum>>1) % d.dim
		if sum&1 == 0 {
			v[bucket]++
		} else {
			v[bucket]--
		}
	}
	return Normalize(v)
}
