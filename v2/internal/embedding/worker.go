package embedding

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"queryforge.local/v2experiment/internal/qferr"
)

// Worker drives the local Python embedding model as a long-lived child process,
// speaking newline-delimited JSON over its stdin and stdout.
//
// ONE PROCESS, MANY REQUESTS. Rule 12 and section 17 of the brief both say this
// outright: spawning an interpreter and loading the model per request would add
// seconds of cost that has nothing to do with the architecture under test, and
// would make every latency number meaningless. The process starts once, at
// NewWorker, and lives until Close.
//
// No socket is opened. Stdio was chosen over localhost HTTP so that "no remote
// inference" is structurally true rather than merely intended — there is no
// port to misconfigure and nothing for another process to connect to.
type Worker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	// mu serialises request/response pairs. The protocol is a single ordered
	// stream: two concurrent callers would interleave their writes and each
	// read the other's reply. A pool of workers is the way to add parallelism,
	// and that is a Phase 9 question to answer with measurements.
	mu sync.Mutex

	identity Identity

	closeOnce sync.Once
	closeErr  error
}

// WorkerOptions configures the child process.
type WorkerOptions struct {
	// Python is the interpreter to run. Defaults to the worker's own virtualenv
	// if present, then to "python3".
	Python string
	// Script is the path to worker.py.
	Script string
	// Model overrides the pinned model id.
	Model string
	// CacheDir overrides where weights are cached.
	CacheDir string
	// StartupTimeout bounds the first handshake, which includes loading the
	// model and — on a cold machine — downloading it.
	StartupTimeout time.Duration
	// Stderr receives the worker's diagnostics. Nil discards them. The worker
	// keeps stdout free of everything but protocol, so this is where load
	// progress and tracebacks go.
	Stderr io.Writer
}

// DefaultModel is the pinned Phase 2 baseline. Chosen as the standard
// lightweight Sentence Transformers model; see
// docs/decisions/007-embedding-stack.md.
const DefaultModel = "sentence-transformers/all-MiniLM-L6-v2"

const defaultStartupTimeout = 180 * time.Second

// NewWorker starts the child process and completes the identity handshake.
//
// It returns only once the model is loaded and has reported its dimension, so a
// successful return means the provider is warm. Every later call is a request
// against an already-loaded model.
func NewWorker(ctx context.Context, opts WorkerOptions) (*Worker, error) {
	script, err := resolveScript(opts.Script)
	if err != nil {
		return nil, err
	}
	python, err := resolvePython(opts.Python, script)
	if err != nil {
		return nil, err
	}

	timeout := opts.StartupTimeout
	if timeout == 0 {
		timeout = defaultStartupTimeout
	}

	cmd := exec.Command(python, script)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	if opts.Model != "" {
		cmd.Env = append(cmd.Env, "QF_V2_EMBEDDING_MODEL="+opts.Model)
	}
	if opts.CacheDir != "" {
		cmd.Env = append(cmd.Env, "QF_V2_MODEL_CACHE="+opts.CacheDir)
	}
	cmd.Stderr = opts.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, qferr.Wrap(qferr.CodeInternal, err, "embedding worker stdin could not be opened")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, qferr.Wrap(qferr.CodeInternal, err, "embedding worker stdout could not be opened")
	}
	if err := cmd.Start(); err != nil {
		return nil, qferr.Wrap(qferr.CodeInternal, err, "embedding worker could not be started (interpreter: %s)", python)
	}

	w := &Worker{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 1<<20),
	}

	handshake, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var resp struct {
		Model      string `json:"model"`
		Revision   string `json:"revision"`
		Dim        int    `json:"dim"`
		Normalized bool   `json:"normalized"`
		Semantic   bool   `json:"semantic"`
	}
	if err := w.roundTrip(handshake, map[string]any{"op": "identity"}, &resp); err != nil {
		_ = w.Close()
		return nil, err
	}
	if resp.Dim <= 0 {
		_ = w.Close()
		return nil, qferr.New(qferr.CodeInternal, "embedding worker reported a non-positive dimension (%d)", resp.Dim)
	}

	w.identity = Identity{
		Model:      resp.Model,
		Revision:   resp.Revision,
		Dim:        resp.Dim,
		Normalized: resp.Normalized,
		Semantic:   resp.Semantic,
	}
	return w, nil
}

func (w *Worker) Identity() Identity { return w.identity }

// PID reports the child process id.
//
// It exists so that "one process, many requests" is assertable rather than
// merely intended: a test can embed repeatedly and check the pid never moves.
// It is also what debug output names when the worker misbehaves.
func (w *Worker) PID() int {
	if w.cmd == nil || w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

func (w *Worker) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var resp struct {
		Vectors [][]float32 `json:"vectors"`
	}
	if err := w.roundTrip(ctx, map[string]any{"op": "embed", "texts": texts}, &resp); err != nil {
		return nil, err
	}

	// The worker must return one vector per text, in order, all of the declared
	// width. A mismatch would silently misalign every vector with the wrong
	// corpus entry — retrieval would still "work" and be wrong everywhere.
	if len(resp.Vectors) != len(texts) {
		return nil, qferr.New(qferr.CodeInternal,
			"embedding worker returned %d vectors for %d texts", len(resp.Vectors), len(texts))
	}
	for i, v := range resp.Vectors {
		if len(v) != w.identity.Dim {
			return nil, qferr.New(qferr.CodeInternal,
				"embedding worker returned a %d-dimension vector at position %d, expected %d",
				len(v), i, w.identity.Dim)
		}
	}
	return resp.Vectors, nil
}

func (w *Worker) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := w.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// Close shuts the child down, politely first.
func (w *Worker) Close() error {
	w.closeOnce.Do(func() {
		// Best effort: if the worker is wedged, the pipe close and Wait below
		// still bring it down.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var ack struct{}
		_ = w.roundTrip(ctx, map[string]any{"op": "shutdown"}, &ack)

		_ = w.stdin.Close()

		done := make(chan error, 1)
		go func() { done <- w.cmd.Wait() }()
		select {
		case err := <-done:
			w.closeErr = err
		case <-time.After(5 * time.Second):
			_ = w.cmd.Process.Kill()
			<-done
			w.closeErr = errors.New("embedding worker did not exit and was killed")
		}
	})
	return w.closeErr
}

// roundTrip writes one request and reads one response, holding the lock for
// both halves so the stream stays ordered.
func (w *Worker) roundTrip(ctx context.Context, request any, out any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	payload, err := json.Marshal(request)
	if err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "embedding request could not be encoded")
	}

	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)

	go func() {
		if _, err := w.stdin.Write(append(payload, '\n')); err != nil {
			done <- result{err: err}
			return
		}
		line, err := w.stdout.ReadBytes('\n')
		done <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		// The goroutine is left to finish against the pipe; Close tears the
		// process down. Returning here without that would leave a half-written
		// request in the stream and desynchronise every later call.
		return qferr.Wrap(qferr.CodeInternal, ctx.Err(), "embedding worker did not respond in time")
	case res := <-done:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) {
				return qferr.Wrap(qferr.CodeInternal, res.err,
					"embedding worker exited unexpectedly; run with --debug to see its output")
			}
			return qferr.Wrap(qferr.CodeInternal, res.err, "embedding worker communication failed")
		}

		var envelope struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(res.line, &envelope); err != nil {
			return qferr.Wrap(qferr.CodeInternal, err, "embedding worker sent a malformed response")
		}
		if !envelope.OK {
			// The worker's message describes its own failure — a missing
			// dependency, a model that would not load. It names no config
			// values, so it is safe in default output.
			return qferr.New(qferr.CodeInternal, "embedding worker failed: %s", envelope.Error)
		}
		if err := json.Unmarshal(res.line, out); err != nil {
			return qferr.Wrap(qferr.CodeInternal, err, "embedding worker response could not be decoded")
		}
		return nil
	}
}

// resolveScript locates worker.py, defaulting to the copy in the module.
func resolveScript(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", qferr.Wrap(qferr.CodeInvalidInput, err, "embedding worker script not found: %s", path)
		}
		return path, nil
	}
	for _, candidate := range []string{
		"embedding_worker/worker.py",
		filepath.Join("..", "embedding_worker", "worker.py"),
		filepath.Join("..", "..", "embedding_worker", "worker.py"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Abs(candidate)
		}
	}
	return "", qferr.New(qferr.CodeInvalidInput,
		"embedding worker script not found; pass its path explicitly")
}

// resolvePython picks the interpreter, preferring the worker's own virtualenv.
//
// Preferring the venv is what makes the experiment reproducible: the system
// python3 may be a different version with a different — or missing —
// sentence-transformers, and a silent fallback to it would produce vectors from
// a stack nobody recorded.
func resolvePython(python, script string) (string, error) {
	if python != "" {
		return python, nil
	}
	venv := filepath.Join(filepath.Dir(script), ".venv", "bin", "python")
	if _, err := os.Stat(venv); err == nil {
		return venv, nil
	}
	found, err := exec.LookPath("python3")
	if err != nil {
		return "", qferr.Wrap(qferr.CodeInvalidInput, err,
			"no python3 on PATH and no virtualenv at %s; see embedding_worker/README.md", venv)
	}
	return found, nil
}

// WorkerAvailable reports whether a worker could plausibly start, and why not
// when it could not. Tests use it to skip rather than fail on a machine where
// the environment was never created.
func WorkerAvailable(script string) (bool, string) {
	resolved, err := resolveScript(script)
	if err != nil {
		return false, "worker.py not found"
	}
	venv := filepath.Join(filepath.Dir(resolved), ".venv", "bin", "python")
	if _, err := os.Stat(venv); err != nil {
		return false, fmt.Sprintf("no virtualenv at %s (run: uv venv --python 3.13 embedding_worker/.venv && uv pip install -r embedding_worker/requirements.txt)", venv)
	}
	return true, ""
}

// ModelSummary is a short label for reports.
func ModelSummary(id Identity) string {
	name := id.Model
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
