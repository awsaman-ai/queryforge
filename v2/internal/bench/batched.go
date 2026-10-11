package bench

import (
	"context"
	"sync/atomic"

	"queryforge.local/v2experiment/internal/embedding"
)

// BatchLimit is the embedding worker's own cap on one request
// (embedding_worker/worker.py, MAX_BATCH). It is a protocol limit, not a
// model limit: a larger batch is refused outright rather than split.
const BatchLimit = 2048

// Batched wraps a provider so that a large Embed call is split into
// worker-sized chunks and stitched back together in order.
//
// This exists because of a real limit the scale matrix ran into:
// retrieval.Build embeds the whole corpus in ONE call, by design (batching is
// why the Provider interface has a batch method at all), and the worker
// refuses a batch over 2,048 texts. A 1,000-entry schema is already 2,576
// surfaces, so nothing past the smallest scale could be indexed at all.
//
// The wrapper is in the benchmark package rather than in the engine on
// purpose. It is a workaround for a Phase 2 limitation found by Phase 9
// measurement; where the fix belongs -- the worker, retrieval.Build, or an
// explicit chunking provider in the shipped path -- is a decision for the
// Phase 9 gate, not something to slip into closed code while benchmarking.
// Chunking changes nothing about the vectors: the model embeds each text
// independently, so the concatenation is identical to a single call.
type Batched struct {
	Provider embedding.Provider
	// Size is the maximum texts per call. Zero means BatchLimit.
	Size int
	// calls counts the worker round trips made, so the report can say how
	// many an index build actually cost. Atomic: the concurrency scenarios
	// drive one wrapped provider from many goroutines, and the race detector
	// caught a plain int here.
	calls atomic.Int64
}

// NewBatched wraps a provider at the worker's own limit.
func NewBatched(p embedding.Provider) *Batched { return &Batched{Provider: p, Size: BatchLimit} }

// Calls reports how many round trips the wrapped provider has been asked for.
func (b *Batched) Calls() int64 { return b.calls.Load() }

func (b *Batched) Identity() embedding.Identity { return b.Provider.Identity() }

func (b *Batched) Close() error { return b.Provider.Close() }

func (b *Batched) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	b.calls.Add(1)
	return b.Provider.EmbedOne(ctx, text)
}

func (b *Batched) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	size := b.Size
	if size <= 0 {
		size = BatchLimit
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += size {
		end := start + size
		if end > len(texts) {
			end = len(texts)
		}
		b.calls.Add(1)
		chunk, err := b.Provider.Embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}
