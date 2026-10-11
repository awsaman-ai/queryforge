// Package embedding defines the boundary between the experiment and whatever
// maps text to vectors.
//
// Rule 11 of the brief allows a LOCAL embedding model precisely because the
// experiment is about semantic vector retrieval — but requires it to sit behind
// an interface so it can be swapped or removed. This package is that interface.
// Nothing above it may know whether the vectors came from a neural model, a
// hashing scheme, or a fixture.
//
// No implementation here may call a generative model, and none may reach the
// network on the request path.
package embedding

import (
	"context"
	"fmt"
	"math"
)

// Identity pins exactly what produced a set of vectors.
//
// It is stored alongside every cached index and reported in every result table.
// Section 25 requires a benchmark to record the model and its version; without
// that, a retrieval number is an anecdote — vectors from two different models
// are not comparable, and silently mixing them produces scores that look
// plausible and mean nothing.
type Identity struct {
	Model    string `json:"model"`
	Revision string `json:"revision"`
	Dim      int    `json:"dim"`
	// Normalized reports whether vectors are unit length. When true, cosine
	// similarity is a plain dot product.
	Normalized bool `json:"normalized"`
	// Semantic distinguishes a real embedding model from a deterministic
	// stand-in. Research results may only be recorded from a semantic provider;
	// see the guard in the CLI.
	Semantic bool `json:"semantic"`
}

// String renders the identity for reports and debug output.
func (i Identity) String() string {
	kind := "non-semantic"
	if i.Semantic {
		kind = "semantic"
	}
	rev := i.Revision
	if rev == "" {
		rev = "unknown"
	}
	return fmt.Sprintf("%s@%s (dim=%d, %s)", i.Model, rev, i.Dim, kind)
}

// Provider maps text to vectors.
//
// Implementations must be safe for concurrent use: a provider is built once at
// startup and shared by every request (section 22, rule 9).
type Provider interface {
	// Identity describes the model. It must not change over a provider's life.
	Identity() Identity

	// Embed maps a batch of texts to vectors, in order. Used when building the
	// index, where the corpus is embedded in one pass.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// EmbedOne maps a single text. Used on the request path, where there is one
	// query and latency is what matters.
	EmbedOne(ctx context.Context, text string) ([]float32, error)

	// Close releases whatever the provider holds — for the worker, the child
	// process. Safe to call more than once.
	Close() error
}

// Cosine returns the cosine similarity of two vectors.
//
// It does NOT assume normalised input: a provider that reports Normalized=false
// would otherwise produce silently wrong scores. The extra magnitude work is
// negligible against the cost of producing the vectors in the first place, and
// Phase 9 can specialise the hot path once profiling says it matters — rule 7
// of section 22 is to measure before optimising.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Normalize scales v to unit length in place and returns it. A zero vector is
// left alone rather than producing NaNs that would propagate into every score
// it touches.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}
