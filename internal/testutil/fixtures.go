// Fixtures shared by more than one package's tests.
//
// These constants and generator helpers used to sit at the top of whichever
// root test file happened to need them first (ast_test.go, generate_test.go,
// security_t_test.go, nested_test.go, bugs_t_test.go). Once those test files
// moved next to the code they exercise, several of them ended up in different
// packages, so the fixtures are hoisted here to keep a single source of truth.

package testutil

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/ast"
	"github.com/awsaman-ai/queryforge/internal/config"
	"github.com/awsaman-ai/queryforge/internal/gen"
)

// CanonicalAST is the running example from the design doc (§7). It exercises
// every Value kind: enum, boolean, relative_date and array.
const CanonicalAST = `{
  "version": "1.0",
  "entity": "Order",
  "filter": {
    "type": "logical", "op": "AND",
    "children": [
      {"type":"comparison","field":"status",   "operator":"equals",      "value":{"kind":"enum","v":"DELIVERED"}},
      {"type":"comparison","field":"refunded", "operator":"equals",      "value":{"kind":"boolean","v":false}},
      {"type":"comparison","field":"createdAt","operator":"after",       "value":{"kind":"relative_date","unit":"day","amount":-30}},
      {"type":"comparison","field":"tags",     "operator":"containsAll", "value":{"kind":"array","v":["premium","express"]}}
    ]
  },
  "sort": [{"field":"createdAt","dir":"DESC"}],
  "limit": 50,
  "offset": 0
}`

// GenConfigJSON carries physical mappings and index/priority hints so the
// generator golden tests can assert real column names and predicate ordering.
const GenConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED","REFUNDED"],
     "operators":["equals","notEquals","in","notIn","isNull","isNotNull"],
     "indexed":true,"priority":10,"mapping":{"sql":"status","mongo":"status"}},
    {"name":"refunded","type":"boolean","mapping":{"sql":"refunded","mongo":"refunded"}},
    {"name":"createdAt","type":"date","operators":["before","after","between"],
     "indexed":true,"priority":8,"mapping":{"sql":"created_at","mongo":"createdAt"}},
    {"name":"amount","type":"number","operators":["gt","lt","gte","lte","between","in"],
     "mapping":{"sql":"amount","mongo":"amount"}},
    {"name":"tags","type":"array","itemType":"string","operators":["contains","containsAny","containsAll"],
     "mapping":{"sql":"tags","mongo":"tags"}},
    {"name":"customerName","type":"string","operators":["contains","startsWith","endsWith","equals","regex"],
     "searchable":true,"mapping":{"sql":"customer_name","mongo":"customerName"}}
  ],
  "defaults":{"limit":50,"maxLimit":500}
}`

// SecConfigJSON is the entity the security tests attack.
const SecConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED"],
     "operators":["equals","notEquals","in","notIn"],
     "indexed":true,"mapping":{"sql":"status","mongo":"status"}},
    {"name":"customerName","type":"string",
     "operators":["equals","contains","startsWith","regex"],
     "mapping":{"sql":"customer_name","mongo":"customerName"}},
    {"name":"amount","type":"number","operators":["gt","lt","between","in"],
     "mapping":{"sql":"amount","mongo":"amount"}},
    {"name":"ssn","type":"string","returnable":false,
     "mapping":{"sql":"ssn","mongo":"ssn"}},
    {"name":"internalNote","type":"string","queryable":false,
     "mapping":{"sql":"internal_note","mongo":"internalNote"}},
    {"name":"tenantId","type":"string","queryable":false,
     "mapping":{"sql":"tenant_id","mongo":"tenantId"}},
    {"name":"sortOrder","type":"number","operators":["gt","lt"],
     "mapping":{"sql":"order","mongo":"order"}}
  ],
  "defaults":{"limit":50,"maxLimit":500}
}`

// FixedNow makes relative-date resolution deterministic across runs.
var FixedNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// GenConfig parses GenConfigJSON.
func GenConfig(t *testing.T) *config.Config { return MustParse(t, GenConfigJSON) }

// GenSQL runs the SQL generator over q with a pinned clock.
func GenSQL(t *testing.T, c *config.Config, q *ast.Query) *gen.Result {
	t.Helper()
	r, err := gen.SQLGenerator{}.Generate(q, c, gen.GenOptions{Now: FixedNow})
	if err != nil {
		t.Fatalf("sql generate: %v", err)
	}
	return r
}

// GenMongo runs the Mongo generator over q with a pinned clock.
func GenMongo(t *testing.T, c *config.Config, q *ast.Query) *gen.MongoQuery {
	t.Helper()
	r, err := gen.MongoGenerator{}.Generate(q, c, gen.GenOptions{Now: FixedNow})
	if err != nil {
		t.Fatalf("mongo generate: %v", err)
	}
	mq, ok := r.Doc.(*gen.MongoQuery)
	if !ok {
		t.Fatalf("mongo generate: Doc is %T, want *gen.MongoQuery", r.Doc)
	}
	return mq
}

// TGen runs the registry-resolved generator for backend over q.
func TGen(t *testing.T, c *config.Config, q *ast.Query, backend string) *gen.Result {
	t.Helper()
	g, ok := gen.DefaultRegistry().Get(backend)
	if !ok {
		t.Fatalf("no generator for %q", backend)
	}
	r, err := g.Generate(q, c, gen.GenOptions{Now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("generate %s: %v", backend, err)
	}
	return r
}

// FilterJSON marshals a Mongo query's filter document for comparison.
func FilterJSON(t *testing.T, mq *gen.MongoQuery) string {
	t.Helper()
	b, err := json.Marshal(mq.Filter)
	if err != nil {
		t.Fatalf("marshal filter: %v", err)
	}
	return string(b)
}

// CanonicalQuery is the design-doc §16 example, built programmatically.
func CanonicalQuery() *ast.Query {
	q := ast.NewQuery("Order")
	q.Filter = And(
		Comp("status", ast.OpEquals, VEnum("DELIVERED")),
		Comp("refunded", ast.OpEquals, VBool(false)),
		Comp("createdAt", ast.OpAfter, VRel("day", -30)),
		Comp("tags", ast.OpContainsAll, VArr("premium", "express")),
	)
	q.Sort = []ast.SortSpec{{Field: "createdAt", Dir: "DESC"}}
	q.Limit = ast.IntPtr(50)
	return q
}
