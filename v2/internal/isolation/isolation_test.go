// Package isolation holds no code. It holds the test that keeps the experiment's
// two hardest rules enforceable by machine rather than by memory:
//
//	rule 6  — do not depend on production QueryForge packages
//	rule 7  — do not make a database connection
//	rule 10 — do not call a remote generative model
//
// Those rules are easy to state and easy to break in month three. A build-time
// check costs nothing and fails loudly the moment an import appears.
package isolation

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbidden maps an import prefix to why it may not appear. Matching is by
// prefix so that subpackages ("github.com/awsaman-ai/queryforge/internal/ast")
// are caught along with the root.
//
// This is a denylist of the specific things the brief forbids, not an allowlist
// of the standard library: net/http is deliberately absent because Phase 2 may
// legitimately talk to a LOCAL embedding worker over loopback. What is banned is
// a remote model or a database, not sockets.
var forbidden = map[string]string{
	"github.com/awsaman-ai/queryforge": "rule 6: v2 must not import production QueryForge packages",

	"database/sql":                        "rule 7: the experiment generates queries and must not connect to a database",
	"github.com/lib/pq":                   "rule 7: no database driver",
	"github.com/jackc/pgx":                "rule 7: no database driver",
	"github.com/go-sql-driver/mysql":      "rule 7: no database driver",
	"go.mongodb.org/mongo-driver":         "rule 7: no database driver",
	"github.com/elastic/go-elasticsearch": "rule 7: no database driver",

	"github.com/anthropics":                "rule 10: no remote generative model",
	"github.com/sashabaranov/go-openai":    "rule 10: no remote generative model",
	"github.com/aws/aws-sdk-go-v2/service": "rule 10: no remote generative model",
	"github.com/ollama/ollama":             "rule 10: no remote generative model",
	"google.golang.org/genai":              "rule 10: no remote generative model",
}

func TestNoForbiddenImports(t *testing.T) {
	// The module root is two levels up from internal/isolation.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}

	fset := token.NewFileSet()
	var scanned int

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds fixtures, not compiled code, and the go tool
			// already ignores it.
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		scanned++

		rel, _ := filepath.Rel(root, path)
		for _, spec := range file.Imports {
			imported, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				t.Errorf("%s: unparsable import %s", rel, spec.Path.Value)
				continue
			}
			for prefix, why := range forbidden {
				if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
					t.Errorf("%s imports %q — %s", rel, imported, why)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// A walk that silently matched nothing would pass forever while checking
	// nothing, which is the failure mode this kind of test is prone to.
	if scanned == 0 {
		t.Fatal("scanned no Go files; the guard is not actually looking at the module")
	}
	t.Logf("scanned %d Go files for forbidden imports", scanned)
}
