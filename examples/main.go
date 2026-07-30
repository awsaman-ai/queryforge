// Command qf is a tiny CLI demonstrating the QueryForge library: it loads a
// config, turns a natural-language request into a validated AST, and compiles
// that AST to a backend query — printing the AST, a prose explanation, and the
// final query. It is intentionally thin; all the logic lives in the library.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	qf "github.com/awsaman-ai/queryforge"
)

func main() {
	// Command-line flags.
	cfgPath := flag.String("config", "examples/orders.config.json", "path to a JSON config file")
	backend := flag.String("backend", "sql", "target backend: sql | mongo")
	text := flag.String("text", "", "natural-language query (required unless -ast is given)")
	astPath := flag.String("ast", "", "path to a JSON AST file: compile it deterministically (no model call)")
	timeout := flag.Duration("timeout", 30*time.Second, "model call timeout")

	// Caller-supplied scope filters, repeatable: -scope key=value. These stand in
	// for values a real application would take from the session (subscription,
	// tenant, user), not from the user's question.
	var scopeArgs scopeFlag
	flag.Var(&scopeArgs, "scope", "extra filter AND-ed into every query, as key=value (repeatable); "+
		"the value is parsed as JSON when it looks like JSON, else as a string")
	scopeInAST := flag.Bool("scope-in-ast", false, "show the injected scope predicates inside the printed AST")
	flag.Parse()

	// Load the config (validates structure and rejects typos).
	cfg, err := qf.LoadConfig(*cfgPath)
	if err != nil {
		die("load config: %v", err)
	}
	engine := qf.New(cfg)
	engine.ScopeInAST = *scopeInAST

	scope := scopeArgs.scope() // nil when no -scope flag was given

	// Deterministic path: compile a hand-written AST with no model call.
	if *astPath != "" {
		runFromAST(engine, *astPath, *backend, scope)
		return
	}

	if strings.TrimSpace(*text) == "" {
		die("provide -text \"...\" (or -ast file.json for the offline path)")
	}

	// Friendly pre-flight: the model call needs the API key named in the config.
	if cfg.Model.APIKeyEnv != "" && os.Getenv(cfg.Model.APIKeyEnv) == "" {
		fmt.Fprintf(os.Stderr, "warning: env var %s is empty; the model call will fail. Export your key:\n  export %s=<your-key>\n\n",
			cfg.Model.APIKeyEnv, cfg.Model.APIKeyEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Full pipeline: NL -> AST -> validate -> generate.
	res, err := engine.Translate(ctx, *text, *backend, scope)
	if err != nil {
		// A refusal is a legitimate answer ("this config can't express that"),
		// not a crash — report it plainly and exit 2 so scripts can tell the
		// two apart.
		var unsupported *qf.UnsupportedRequestError
		if errors.As(err, &unsupported) {
			fmt.Printf("Request: %s\n\n", *text)
			fmt.Printf("Cannot answer with this config: %s\n", unsupported.Reason)
			fmt.Println("\nNo query was generated. Add the missing field to the config, or rephrase using a configured field.")
			os.Exit(2)
		}
		die("translate: %v", err)
	}

	fmt.Printf("Request: %s\n\n", *text)
	printAST(res.AST)
	fmt.Printf("\nExplain:\n  %s\n", res.Explain)
	printQuery(res.Query)
	printScope(res.Scope)
	printWarnings(res.Warnings)
	if res.ProviderUsed != "" { // populated when a fallback chain is configured
		fmt.Printf("\nAnswered by: %s\n", res.ProviderUsed)
	}
	if res.RepairAttempts > 0 {
		fmt.Printf("(recovered after %d repair attempt(s))\n", res.RepairAttempts)
	}
}

// runFromAST loads an AST from disk and compiles it deterministically.
func runFromAST(engine *qf.Engine, path, backend string, scope qf.Scope) {
	data, err := os.ReadFile(path)
	if err != nil {
		die("read ast: %v", err)
	}
	var ast qf.Query
	if err = json.Unmarshal(data, &ast); err != nil {
		die("parse ast: %v", err)
	}
	q, err := engine.GenerateFrom(&ast, backend, scope)
	if err != nil {
		die("generate: %v", err)
	}

	// GenerateFrom returns only the query, so ask for the effective AST to print
	// and explain — otherwise the readback would omit the scope that was applied.
	effective, filters, err := engine.ApplyScope(&ast, scope)
	if err != nil {
		die("scope: %v", err)
	}
	shown := &ast
	if engine.ScopeInAST {
		shown = effective
	}

	printAST(shown)
	fmt.Printf("\nExplain:\n  %s\n", qf.Explain(effective, engine.Config()))
	printQuery(q)
	printScope(filters)
	printWarnings(q.Warnings)
}

// scopeFlag collects repeated -scope key=value arguments.
type scopeFlag []string

// String renders the accumulated pairs (required by flag.Value).
func (s *scopeFlag) String() string { return strings.Join(*s, ",") }

// Set records one -scope occurrence.
func (s *scopeFlag) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	*s = append(*s, v)
	return nil
}

// scope parses the collected pairs into a qf.Scope. Each value is first tried as
// JSON, so numbers, booleans and lists arrive with their real types
// (-scope userId=9, -scope 'enterpriseId=["E-1","E-2"]'); anything that is not
// valid JSON is taken as a plain string, which is what bare ids like SUB-42 are.
func (s *scopeFlag) scope() qf.Scope {
	if len(*s) == 0 {
		return nil // no -scope flags: inject nothing
	}
	out := make(qf.Scope, len(*s))
	for _, pair := range *s {
		key, raw, _ := strings.Cut(pair, "=") // Set guaranteed the separator
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			v = raw // not JSON: treat it as the string it is
		}
		out[strings.TrimSpace(key)] = v
	}
	return out
}

// printScope lists the filters that were forced onto the query.
func printScope(filters []qf.ScopeFilter) {
	if len(filters) == 0 {
		return
	}
	fmt.Println("\nScope applied:")
	for _, f := range filters {
		origin := "not in config" // the usual case: a session value, not query vocabulary
		if f.Declared {
			origin = "declared in config"
		}
		fmt.Printf("  - %s  (%s)\n", f, origin)
	}
}

// printAST pretty-prints the AST as JSON.
func printAST(ast *qf.Query) {
	fmt.Println("AST:")
	b, err := json.MarshalIndent(ast, "  ", "  ")
	if err != nil {
		die("render ast: %v", err)
	}
	fmt.Printf("  %s\n", b)
}

// printQuery renders the compiled query for the appropriate backend.
func printQuery(q *qf.Result) {
	fmt.Printf("\nQuery (%s):\n", q.Backend)
	if q.SQL != "" {
		fmt.Printf("  %s\n", q.SQL)
		if len(q.Args) > 0 {
			fmt.Printf("  args: %v\n", q.Args)
		}
		return
	}
	b, err := json.MarshalIndent(q.Doc, "  ", "  ")
	if err != nil {
		die("render query: %v", err)
	}
	fmt.Printf("  %s\n", b)
}

// printWarnings lists any non-fatal advisories.
func printWarnings(warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Println("\nWarnings:")
	for _, w := range warnings {
		fmt.Printf("  - %s\n", w)
	}
}

// die prints an error to stderr and exits non-zero.
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
