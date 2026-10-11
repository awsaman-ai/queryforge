package main

import (
	"testing"

	"queryforge.local/v2experiment/internal/compiler"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/qferr"
)

// Phase 8's REFUSED expectation (decision 021): any explicit refusal passes,
// and a query never does — the adversarial gate's "explicit rejection".
func TestE2EExpectRefused(t *testing.T) {
	c := e2eCase{ID: "T", Input: "x", Expect: expectRefused}
	for _, code := range []qferr.Code{qferr.CodeSecurityRejected, qferr.CodeNoSemanticMatch, qferr.CodeUnsupportedQuery} {
		if row := evaluateE2E(c, pipeline.Result{Stage: pipeline.StageRank}, qferr.New(code, "refused")); !row.pass {
			t.Errorf("%s: a refusal did not pass REFUSED", code)
		}
	}
	res := pipeline.Result{Stage: pipeline.StageDone, Query: compiler.Query{SQL: "SELECT 1;"}}
	if row := evaluateE2E(c, res, nil); row.pass {
		t.Error("a query passed REFUSED")
	}
	// A specific code is still exact.
	c.Expect = string(qferr.CodeSecurityRejected)
	if row := evaluateE2E(c, pipeline.Result{}, qferr.New(qferr.CodeNoSemanticMatch, "x")); row.pass {
		t.Error("a specific expectation accepted another code")
	}
}

// Control and invisible format characters are shown, escaped, in the report:
// a NUL would otherwise make the golden a binary file, and a zero-width space
// or a right-to-left override would hide what the input held.
func TestReportShowsInvisibleCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"a\x00b":       "`a\\u0000b`",
		"a​b":          "`a\\u200bb`",
		"‮ab":          "`\\u202eab`",
		"plain | pipe": "`plain \\| pipe`",
		"café 😀":       "`café 😀`",
	} {
		if got := mdCell(in); got != want {
			t.Errorf("mdCell(%q) = %s, want %s", in, got, want)
		}
	}
}
