package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Decision 022 §3, at the public surface go_test_qf's POST /v1/noLLM/tosql
// calls: a request with SQL statement or comment syntax comes back as a coded
// refusal and an EMPTY Result — no SQL and no parameters — so no caller can
// execute anything for it. The deterministic embedder is enough: the request
// is refused before the model would be asked anything.
func TestStatementSyntaxIsRefusedWithNoQuery(t *testing.T) {
	ctx := context.Background()
	eng, err := Open(ctx, Options{
		ConfigPath: filepath.Join("..", "testdata", "configs", "commerce.json"),
		Embedder:   EmbedderDeterministic,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	for _, in := range []string{
		"customers from India; select * from orders",
		"customers named Smith; --",
		"customers/**/from/**/India",
		"' OR 1=1 --",
	} {
		res, err := eng.ToSQL(ctx, in)
		var qe *Error
		if !errors.As(err, &qe) || qe.Code != "SECURITY_REJECTED" {
			t.Errorf("%q: err = %v, want a SECURITY_REJECTED *Error", in, err)
		}
		if res.SQL != "" || res.Params != nil {
			t.Errorf("%q: a refused request returned a query: %+v", in, res)
		}
	}
}
