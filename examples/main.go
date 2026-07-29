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
	flag.Parse()

	// Load the config (validates structure and rejects typos).
	cfg, err := qf.LoadConfig(*cfgPath)
	if err != nil {
		die("load config: %v", err)
	}
	engine := qf.New(cfg)

	// Deterministic path: compile a hand-written AST with no model call.
	if *astPath != "" {
		runFromAST(engine, *astPath, *backend)
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
	res, err := engine.Translate(ctx, *text, *backend)
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
	printWarnings(res.Warnings)
	if res.ProviderUsed != "" { // populated when a fallback chain is configured
		fmt.Printf("\nAnswered by: %s\n", res.ProviderUsed)
	}
	if res.RepairAttempts > 0 {
		fmt.Printf("(recovered after %d repair attempt(s))\n", res.RepairAttempts)
	}
}

// runFromAST loads an AST from disk and compiles it deterministically.
func runFromAST(engine *qf.Engine, path, backend string) {
	data, err := os.ReadFile(path)
	if err != nil {
		die("read ast: %v", err)
	}
	var ast qf.Query
	if err := json.Unmarshal(data, &ast); err != nil {
		die("parse ast: %v", err)
	}
	q, err := engine.GenerateFrom(&ast, backend)
	if err != nil {
		die("generate: %v", err)
	}
	printAST(&ast)
	fmt.Printf("\nExplain:\n  %s\n", qf.Explain(&ast, engine.Config()))
	printQuery(q)
	printWarnings(q.Warnings)
}

// printAST pretty-prints the AST as JSON.
func printAST(ast *qf.Query) {
	fmt.Println("AST:")
	b, _ := json.MarshalIndent(ast, "  ", "  ")
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
	b, _ := json.MarshalIndent(q.Doc, "  ", "  ")
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
