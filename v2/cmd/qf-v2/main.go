// Command qf-v2 is the CLI for the non-LLM semantic query experiment.
//
// It compiles natural language into a final SQL string plus parameters. It never
// opens a database connection and never executes anything: the caller decides
// whether to run what comes back (brief, sections 7-8 of the rules and 16.9).
//
// PHASE 7: --query runs the whole pipeline (internal/pipeline): extraction,
// retrieval, ranking, binding, the SemanticPlan and the deterministic
// compiler. Until Phase 7 it returned NOT_IMPLEMENTED, on purpose: emitting a
// plausible query from a pipeline that had not been built is the
// self-deception section 29 lists as experiment failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/configlint"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Exit codes. Kept distinct so scripted manual gates can tell "the engine
// refused" (1) from "I typed the command wrong" (2) — a distinction that matters
// once the Phase 10 evaluation harness runs thousands of invocations.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// version identifies the experiment build in reports. It is not a release
// version: the experiment is not published.
const version = "v2-experiment phase-7"

// supportedBackends is the frozen Phase 3 scope from the brief: PostgreSQL-style
// SQL only. MySQL, MongoDB, Elasticsearch and OpenSearch are Phase 11, and only
// if Phase 10 says go. Accepting them now would let a caller believe they work.
var supportedBackends = []string{"postgres"}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run holds the whole CLI so that main_test.go can drive it with explicit
// streams instead of shelling out to a built binary.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("qf-v2", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath     = fs.String("config", "", "path to the semantic config JSON (required)")
		query       = fs.String("query", "", "the natural-language request to compile")
		backend     = fs.String("backend", "postgres", "target dialect; only \"postgres\" is in scope")
		explain     = fs.Bool("explain", false, "print the reasoning behind the chosen query")
		debug       = fs.Bool("debug", false, "print per-phase diagnostics, scores and timings")
		asJSON      = fs.Bool("json", false, "emit machine-readable output")
		validateCfg = fs.Bool("validate-config", false, "load and check the config, then exit without compiling a query")
		showVersion = fs.Bool("version", false, "print the experiment build identifier and exit")

		// Phase 2: the semantic corpus and vector index.
		search       = fs.String("search", "", "retrieve the corpus entries nearest to a phrase")
		topK         = fs.Int("top-k", 10, "how many matches --search reports")
		strategy     = fs.String("strategy", string(retrieval.StrategySurface), "how entries are embedded: surface or passage")
		indexPath    = fs.String("index", "", "path to a cached vector index; read if it exists, written by --build-index")
		buildIndex   = fs.Bool("build-index", false, "embed the corpus and write the index named by --index")
		embedder     = fs.String("embedder", string(embedderWorker), "embedding provider: worker (the local model) or deterministic (hashing, NOT semantic)")
		model        = fs.String("model", "", "override the pinned embedding model id")
		workerScript = fs.String("worker-script", "", "path to embedding_worker/worker.py")
		probe        = fs.String("probe", "", "run a file of query/expected pairs against one warm model and report accuracy")

		// Phase 3: phrase and slot extraction.
		analyze       = fs.String("analyze", "", "show the tokens, numbers, dates and candidate spans for a sentence")
		maxSpanTokens = fs.Int("max-span-tokens", 0, "longest candidate phrase, in tokens (default 4)")

		// Phase 4: candidate ranking.
		rank      = fs.String("rank", "", "rank every candidate reading of a sentence and decide accept / ambiguous / reject")
		rankProbe = fs.String("rank-probe", "", "run a file of sentence/span/expected rows and compare ranking with raw top-1")
		minScore  = fs.Float64("min-score", ranking.DefaultPolicy().MinScore, "ranking: lowest top-1 score to accept")
		minMargin = fs.Float64("min-margin", ranking.DefaultPolicy().MinMargin, "ranking: lowest top-1 minus runner-up margin to accept")

		// Phase 5: semantic binding and the schema graph.
		bind = fs.String("bind", "", "bind a sentence into root entity, joins, conditions and aggregates")

		// Phase 6: the SemanticPlan and the deterministic compiler.
		compile  = fs.String("compile", "", "compile a hand-written SemanticPlan JSON file into SQL and parameters")
		planText = fs.String("plan", "", "show the SemanticPlan the planner builds for a sentence (no SQL until Phase 7)")
		now      = fs.String("now", "", "the request clock for --plan, RFC 3339 (default: the wall clock)")
		tz       = fs.String("tz", "", "the IANA timezone calendar periods are cut in for --plan and --query (default: UTC)")

		// Phase 7: end to end.
		e2e = fs.String("e2e", "", "run a reviewed end-to-end case file through one warm pipeline and report")
	)
	fs.Usage = func() { usage(stderr, fs) }

	if err := fs.Parse(args); err != nil {
		// flag already reported the problem to stderr.
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q; the request goes in --query\n", fs.Arg(0))
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return exitOK
	}

	// Which ranking thresholds were given explicitly. Recorded so the report
	// can say "overridden" — a result under a hand-set threshold must never be
	// mistaken for one under the calibrated policy.
	thresholdsSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "min-score" || f.Name == "min-margin" {
			thresholdsSet = true
		}
	})

	err := execute(options{
		configPath:    *cfgPath,
		query:         *query,
		backend:       *backend,
		explain:       *explain,
		debug:         *debug,
		asJSON:        *asJSON,
		validateCfg:   *validateCfg,
		search:        *search,
		topK:          *topK,
		strategy:      retrieval.Strategy(*strategy),
		indexPath:     *indexPath,
		buildIndex:    *buildIndex,
		embedder:      embedderKind(*embedder),
		model:         *model,
		workerScript:  *workerScript,
		probe:         *probe,
		analyze:       *analyze,
		maxSpanTokens: *maxSpanTokens,
		rank:          *rank,
		rankProbe:     *rankProbe,
		minScore:      *minScore,
		minMargin:     *minMargin,
		thresholdsSet: thresholdsSet,
		bind:          *bind,
		compile:       *compile,
		plan:          *planText,
		now:           *now,
		tz:            *tz,
		e2e:           *e2e,
	}, stdout, stderr)
	if err != nil {
		report(err, *asJSON, *debug, stderr)
		return exitFailure
	}
	return exitOK
}

type options struct {
	configPath  string
	query       string
	backend     string
	explain     bool
	debug       bool
	asJSON      bool
	validateCfg bool

	search       string
	topK         int
	strategy     retrieval.Strategy
	indexPath    string
	buildIndex   bool
	embedder     embedderKind
	model        string
	workerScript string
	probe        string

	analyze       string
	maxSpanTokens int

	rank      string
	rankProbe string
	// minScore and minMargin are the ranking thresholds; their defaults are
	// ranking.DefaultPolicy's, and ranking.Policy.Validate refuses nonsense.
	minScore      float64
	minMargin     float64
	thresholdsSet bool

	bind string

	compile string
	plan    string
	now     string
	tz      string

	e2e string
}

func execute(opt options, stdout, stderr io.Writer) error {
	// --analyze runs before the config check: phrase extraction is
	// config-independent by design. What a span MEANS needs the schema; what a
	// span IS does not, and requiring a config here would imply otherwise.
	if opt.analyze != "" {
		return runAnalyze(opt, stdout)
	}
	if strings.TrimSpace(opt.configPath) == "" {
		return qferr.New(qferr.CodeInvalidInput, "--config is required")
	}
	if !isSupportedBackend(opt.backend) {
		return qferr.New(qferr.CodeInvalidInput,
			"backend %q is outside the experiment scope (supported: %s)",
			opt.backend, strings.Join(supportedBackends, ", "))
	}

	// The config is loaded before the query is examined so that a broken config
	// always reports CONFIG_ERROR, whatever else is wrong with the invocation.
	// Diagnosing the config first is what makes the error code informative.
	cfg, err := config.Load(opt.configPath)
	if err != nil {
		return err
	}

	if opt.validateCfg {
		return emitConfigOK(cfg, opt, stdout)
	}
	// --compile needs only the config: a plan names schema objects, and the
	// compiler resolves them against the registry. No model is loaded.
	if strings.TrimSpace(opt.compile) != "" {
		return runCompile(opt, cfg, stdout)
	}

	// Phase 2 modes. They share the config that was just validated, but build
	// their own registry and corpus, so the config is loaded twice in this
	// path — negligible against embedding, and it keeps the modes independent
	// until Phase 7 assembles a real pipeline.
	if !retrieval.ValidStrategy(opt.strategy) {
		return qferr.New(qferr.CodeInvalidInput, "unknown strategy %q (known: surface, passage)", opt.strategy)
	}
	ctx := context.Background()
	if opt.buildIndex {
		if strings.TrimSpace(opt.indexPath) == "" {
			return qferr.New(qferr.CodeInvalidInput, "--build-index needs --index to say where to write")
		}
		return runBuildIndex(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.probe) != "" {
		return runProbe(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.search) != "" {
		return runSearch(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.rankProbe) != "" {
		return runRankProbe(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.rank) != "" {
		return runRank(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.bind) != "" {
		return runBind(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.plan) != "" {
		return runPlan(ctx, opt, stdout, stderr)
	}
	if strings.TrimSpace(opt.e2e) != "" {
		return runE2E(ctx, opt, stdout, stderr)
	}

	if strings.TrimSpace(opt.query) == "" {
		return qferr.New(qferr.CodeInvalidInput,
			"nothing to do: pass --query, --search, --rank, --bind, --plan, --compile, --e2e, --analyze, --validate-config or --build-index")
	}

	// Phase 7: the whole pipeline, natural language to final query.
	return runQuery(ctx, opt, stdout, stderr)
}

func isSupportedBackend(name string) bool {
	for _, b := range supportedBackends {
		if b == name {
			return true
		}
	}
	return false
}

// emitConfigOK reports a successful load. It prints counts, never values: a
// config carries table names, enum values and value aliases, and
// --validate-config is the one command a user is likely to run against
// production-shaped config.
//
// It builds the schema registry before reporting. Indexing is what would expose
// a config that validates but cannot be indexed, and doing it here means
// --validate-config exercises the same path a real request would.
//
// Phase 8 adds the WARNING channel (internal/configlint): phrases the engine
// will refuse or can never match. They do not fail the load, and the exit
// code stays 0. Only this command reports them; a request stays quiet
// (section 24). Like violations, warnings redact config values unless --debug
// is given.
func emitConfigOK(cfg *config.Config, opt options, stdout io.Writer) error {
	registry := config.NewSchemaRegistry(cfg)
	warnings := configlint.Check(registry)

	if opt.asJSON {
		type warning struct {
			Rule   config.Rule `json:"rule"`
			Path   string      `json:"path"`
			Detail string      `json:"detail"`
		}
		ws := make([]warning, 0, len(warnings))
		for _, w := range warnings {
			d := w.Detail()
			if opt.debug {
				d = w.DetailRevealed()
			}
			ws = append(ws, warning{Rule: w.Rule, Path: w.Path, Detail: d})
		}
		out := struct {
			OK            bool      `json:"ok"`
			Config        string    `json:"config"`
			Entities      int       `json:"entities"`
			Fields        int       `json:"fields"`
			Operators     int       `json:"operators"`
			Aggregations  int       `json:"aggregations"`
			Relationships int       `json:"relationships"`
			QueryPatterns int       `json:"queryPatterns"`
			Warnings      []warning `json:"warnings"`
		}{
			OK: true, Config: opt.configPath, Warnings: ws,
			Entities:      len(registry.Entities()),
			Fields:        len(cfg.Fields),
			Operators:     len(registry.ConfiguredOperators()),
			Aggregations:  len(registry.ConfiguredAggregations()),
			Relationships: len(cfg.Relationships),
			QueryPatterns: len(registry.QueryPatterns()),
		}
		enc, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return qferr.Wrap(qferr.CodeInternal, err, "output could not be encoded")
		}
		fmt.Fprintln(stdout, string(enc))
		return nil
	}
	fmt.Fprintf(stdout, "config OK: %s\n", cfg.Summary())
	if len(warnings) > 0 {
		fmt.Fprintf(stdout, "%d warning(s) — the config loads; each names a phrase the engine will refuse or can never match:\n", len(warnings))
		for _, w := range warnings {
			line := w.String()
			if opt.debug {
				line = w.StringRevealed()
			}
			fmt.Fprintf(stdout, "  %s\n", line)
		}
	}
	return nil
}

// revealer is an error with a second, debug-only rendering that shows what the
// default one withheld. config.ValidationError (Phase 1) and
// ranking.VerdictError (Phase 4) both implement it; an interface rather than
// a type switch, so a later phase's error cannot be forgotten here.
type revealer interface {
	error
	ErrorRevealed() string
	RedactedValueCount() int
}

// report renders a failure. The wrapped cause appears only under --debug,
// because causes can quote file content or user input.
func report(err error, asJSON, debug bool, stderr io.Writer) {
	var coded *qferr.Error
	if !errors.As(err, &coded) {
		// An uncoded error means some path forgot to classify itself. Say so
		// plainly instead of laundering it into a user-facing category.
		coded = qferr.Wrap(qferr.CodeInternal, err, "unclassified failure")
	}

	// A config failure can render twice: redacted by default, with config values
	// shown under --debug. Anything not reached here has no revealed form, and
	// its default message is already the whole story.
	detail := ""
	if debug {
		var rv revealer
		// Only when something was actually withheld. Otherwise the revealed
		// rendering is byte-identical to the default one, and printing both
		// would be noise that teaches people to skim past the debug section.
		if errors.As(err, &rv) && rv.RedactedValueCount() > 0 {
			detail = rv.ErrorRevealed()
		}
	}

	if asJSON {
		var (
			enc    []byte
			encErr error
		)
		if debug {
			enc, encErr = coded.JSONRevealed(detail)
		} else {
			enc, encErr = coded.JSON()
		}
		if encErr != nil {
			fmt.Fprintf(stderr, "%s: %s\n", qferr.CodeInternal, "error could not be encoded")
			return
		}
		fmt.Fprintln(stderr, string(enc))
		return
	}

	fmt.Fprintf(stderr, "%s: %s\n", coded.Code, coded.Message)
	if !debug {
		return
	}
	if detail != "" {
		fmt.Fprintf(stderr, "debug: values revealed:\n%s\n", detail)
	}
	if coded.Wrapped != nil {
		fmt.Fprintf(stderr, "debug: cause: %v\n", coded.Wrapped)
	}
}

func usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `qf-v2 — non-LLM semantic query compiler (research experiment)

Compiles a natural-language request into a final SQL query and parameters.
It never connects to a database and never executes a query.

Usage:
  qf-v2 --config <file> --query <text> [--now RFC3339] [--tz Zone] [--explain] [--debug] [--json]
  qf-v2 --config <file> --e2e <cases.json>                (the Phase 7 reviewed query set)
  qf-v2 --config <file> --validate-config [--json]
  qf-v2 --config <file> --search <phrase> [--top-k N] [--strategy surface|passage] [--debug]
  qf-v2 --config <file> --build-index --index <path>
  qf-v2 --config <file> --probe <file.tsv> [--strategy surface|passage]
  qf-v2 --config <file> --rank <sentence> [--min-score X] [--min-margin Y] [--debug] [--json]
  qf-v2 --config <file> --rank-probe <file.tsv> [--min-score X] [--min-margin Y]
  qf-v2 --config <file> --bind <sentence> [--debug] [--json]
  qf-v2 --config <file> --plan <sentence> [--now RFC3339] [--tz Zone] [--debug]
  qf-v2 --config <file> --compile <plan.json> [--debug] [--json]    (no model needed)
  qf-v2 --analyze <sentence> [--debug] [--json]          (no config needed)

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(w, `
Exit codes:
  0  success
  1  the engine refused or failed; stderr carries a stable error code
  2  the command line itself was wrong
`)
}
