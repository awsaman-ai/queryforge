package embedding

import (
	"context"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests exercise the real local model. They are skipped — not failed —
// when the Python environment has not been created, so a fresh checkout can run
// `go test ./...` without a 100 MB download. The manual gate runs them for real.
func newTestWorker(t *testing.T) *Worker {
	t.Helper()
	if ok, why := WorkerAvailable(""); !ok {
		t.Skipf("embedding worker unavailable: %s", why)
	}

	var stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	w, err := NewWorker(ctx, WorkerOptions{Stderr: &stderr})
	if err != nil {
		t.Fatalf("NewWorker: %v\nworker stderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return w
}

func TestWorkerIdentity(t *testing.T) {
	w := newTestWorker(t)
	id := w.Identity()

	if id.Dim != 384 {
		t.Errorf("dim = %d, want 384 for all-MiniLM-L6-v2", id.Dim)
	}
	if !id.Semantic {
		t.Error("the real model must report itself semantic")
	}
	if !id.Normalized {
		t.Error("the worker normalises vectors, so identity should say so")
	}
	if !strings.Contains(id.Model, "MiniLM") {
		t.Errorf("model = %q, want the pinned MiniLM baseline", id.Model)
	}
	// Revision pins the exact weights. Without it a retrieval number is
	// traceable only to a name that upstream may have re-uploaded.
	if id.Revision == "" || id.Revision == "unknown" {
		t.Errorf("revision = %q, want a resolved commit hash", id.Revision)
	}
}

// The architectural requirement from rule 12 and section 17: the model is
// loaded once and reused. A worker that respawned per request would still
// return correct vectors, so correctness tests cannot catch this — only the pid
// can.
func TestWorkerReusesOneProcess(t *testing.T) {
	w := newTestWorker(t)
	pid := w.PID()
	if pid == 0 {
		t.Fatal("worker has no process")
	}

	for i := 0; i < 5; i++ {
		if _, err := w.EmbedOne(context.Background(), "customer spending"); err != nil {
			t.Fatalf("EmbedOne: %v", err)
		}
		if w.PID() != pid {
			t.Fatalf("process changed from %d to %d: the model is being reloaded", pid, w.PID())
		}
	}

	// And the reuse should show in the timing: an embed against a warm model
	// is milliseconds, while loading one is seconds.
	start := time.Now()
	if _, err := w.EmbedOne(context.Background(), "money spent"); err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a warm embed took %v; the model may be reloading per request", elapsed)
	}
}

func TestWorkerVectorShape(t *testing.T) {
	w := newTestWorker(t)
	texts := []string{"customer spending", "money spent", "client location"}

	vectors, err := w.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(vectors), len(texts))
	}
	for i, v := range vectors {
		if len(v) != w.Identity().Dim {
			t.Errorf("vector %d has %d dimensions, want %d", i, len(v), w.Identity().Dim)
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if math.Abs(sum-1) > 1e-4 {
			t.Errorf("vector %d is not unit length: |v|² = %f", i, sum)
		}
	}
}

// The minimum bar for the whole experiment. If a paraphrase does not outscore
// an unrelated field, the semantic layer is not doing the job the architecture
// assigns to it, and Phase 2 should fail rather than proceed.
func TestWorkerCapturesParaphrase(t *testing.T) {
	w := newTestWorker(t)
	ctx := context.Background()

	vectors, err := w.Embed(ctx, []string{
		"Total amount of money spent by the customer.",
		"customer spending",
		"The country where the customer is located.",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	spentDesc, spending, country := vectors[0], vectors[1], vectors[2]

	related := Cosine(spentDesc, spending)
	unrelated := Cosine(spentDesc, country)

	if related <= unrelated {
		t.Errorf("a paraphrase should outscore an unrelated field: %.3f vs %.3f", related, unrelated)
	}
	t.Logf("paraphrase %.3f vs unrelated %.3f (margin %.3f)", related, unrelated, related-unrelated)
}

func TestWorkerBatchMatchesSingle(t *testing.T) {
	w := newTestWorker(t)
	ctx := context.Background()

	batch, err := w.Embed(ctx, []string{"customer spending"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	single, err := w.EmbedOne(ctx, "customer spending")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	// Index build uses the batch path and queries use the single path; if they
	// disagreed, every score would be subtly wrong.
	if sim := Cosine(batch[0], single); sim < 0.9999 {
		t.Errorf("batch and single embeddings disagree: cosine %.6f", sim)
	}
}

// The protocol is one ordered stream, so concurrent callers must be serialised.
// Run under -race, this is what proves they are.
func TestWorkerIsConcurrencySafe(t *testing.T) {
	w := newTestWorker(t)

	reference, err := w.EmbedOne(context.Background(), "customer spending")
	if err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := w.EmbedOne(context.Background(), "customer spending")
			if err != nil {
				errs <- err
				return
			}
			// Interleaved writes would hand a caller someone else's reply, so
			// a wrong vector here is the symptom of a desynchronised stream.
			if Cosine(v, reference) < 0.9999 {
				errs <- errMismatch
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent embed: %v", err)
	}
}

var errMismatch = errorString("concurrent call received a different vector for the same text")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestWorkerRejectsMissingScript(t *testing.T) {
	_, err := NewWorker(context.Background(), WorkerOptions{Script: "/nonexistent/worker.py"})
	if err == nil {
		t.Fatal("a missing script was accepted")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, should say the script was not found", err)
	}
}

func TestWorkerCloseIsIdempotent(t *testing.T) {
	if ok, why := WorkerAvailable(""); !ok {
		t.Skipf("embedding worker unavailable: %s", why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	w, err := NewWorker(ctx, WorkerOptions{Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	first := w.Close()
	if second := w.Close(); second != first {
		t.Errorf("Close is not idempotent: %v then %v", first, second)
	}
}
