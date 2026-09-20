package gen_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/testutil"

	. "github.com/awsaman-ai/queryforge/internal/ast"
	. "github.com/awsaman-ai/queryforge/internal/config"
	. "github.com/awsaman-ai/queryforge/internal/gen"
)

// periodConfig is one date field reachable from every backend, with an
// optional timezone fragment (e.g. `"timezone":"Asia/Kolkata",`).
func periodConfig(t *testing.T, tzFragment string) *Config {
	return testutil.MustParse(t, `{
  "entity":"Order","model":{},`+tzFragment+`
  "backends":{"sql":{"table":"orders"},"mysql":{"table":"orders"},
              "mongo":{"collection":"orders"},"elasticsearch":{"index":"orders"}},
  "fields":[
    {"name":"createdAt","type":"date","operators":["before","after","between"],
     "mapping":{"sql":"created_at","mysql":"created_at","mongo":"createdAt","elasticsearch":"createdAt"}},
    {"name":"status","type":"enum","values":["PLACED","DELIVERED"],"operators":["equals"],
     "mapping":{"sql":"status","mysql":"status","mongo":"status","elasticsearch":"status"}}
  ],
  "defaults":{"limit":50,"maxLimit":500}
}`)
}

// yesterdayUTC is day -1..-1 seen from testutil.FixedNow (2026-07-28 12:00Z).
var (
	yesterdayStart = time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	yesterdayEnd   = time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
)

// TestPeriodSQL pins the half-open rendering on both SQL dialects: >= start AND
// < end, parenthesized, with both instants bound as arguments — never BETWEEN,
// whose inclusive upper end would take in the first instant of today.
func TestPeriodSQL(t *testing.T) {
	c := periodConfig(t, "")
	q := testutil.Single(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -1)))

	pg := testutil.GenSQL(t, c, q)
	if want := "SELECT * FROM orders WHERE (created_at >= $1 AND created_at < $2) LIMIT 50"; pg.SQL != want {
		t.Errorf("postgres:\n got %s\nwant %s", pg.SQL, want)
	}
	if !reflect.DeepEqual(pg.Args, []any{yesterdayStart, yesterdayEnd}) {
		t.Errorf("postgres args = %v", pg.Args)
	}

	my, err := MySQLGenerator{}.Generate(q, c, GenOptions{Now: testutil.FixedNow})
	if err != nil {
		t.Fatalf("mysql: %v", err)
	}
	if want := "SELECT * FROM orders WHERE (created_at >= ? AND created_at < ?) LIMIT 50"; my.SQL != want {
		t.Errorf("mysql:\n got %s\nwant %s", my.SQL, want)
	}
	if !reflect.DeepEqual(my.Args, []any{yesterdayStart, yesterdayEnd}) {
		t.Errorf("mysql args = %v", my.Args)
	}
}

// TestPeriodSQLUnderNotAndOr checks the pair stays one predicate: NOT must
// negate the whole range, and an OR sibling must not split it.
func TestPeriodSQLUnderNotAndOr(t *testing.T) {
	c := periodConfig(t, "")
	q := NewQuery("Order")
	q.Filter = testutil.Or(
		testutil.Not(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("month", -1, -1))),
		testutil.Comp("status", OpEquals, testutil.VEnum("PLACED")),
	)
	r := testutil.GenSQL(t, c, q)
	if !strings.Contains(r.SQL, "NOT ((created_at >= $1 AND created_at < $2))") {
		t.Errorf("NOT does not wrap the whole range: %s", r.SQL)
	}
}

// TestPeriodMongo pins the Mongo shape: $gte / $lt on real dates.
func TestPeriodMongo(t *testing.T) {
	c := periodConfig(t, "")
	mq := testutil.GenMongo(t, c, testutil.Single(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -1))))
	got := testutil.FilterJSON(t, mq)
	if want := `{"createdAt":{"$gte":"2026-07-27T00:00:00Z","$lt":"2026-07-28T00:00:00Z"}}`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
	rng := mq.Filter["createdAt"].(map[string]any)
	if _, ok := rng["$gte"].(time.Time); !ok {
		t.Errorf("$gte must be a time.Time (a BSON date), got %T — a string compares as text", rng["$gte"])
	}
}

// TestPeriodElasticsearch pins the range query: gte / lt as RFC3339 strings.
func TestPeriodElasticsearch(t *testing.T) {
	c := periodConfig(t, "")
	eq := genES(t, c, testutil.Single(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -1))))
	got, _ := json.Marshal(eq.Query)
	if !strings.Contains(string(got), `"range":{"createdAt":{"gte":"2026-07-27T00:00:00Z","lt":"2026-07-28T00:00:00Z"}}`) {
		t.Errorf("query = %s", got)
	}
}

// TestPeriodHonoursTimezone runs the same AST under an Asia/Kolkata config on
// all three backends: "yesterday" must start at IST midnight (18:30Z), and
// every backend must agree on the instants.
func TestPeriodHonoursTimezone(t *testing.T) {
	c := periodConfig(t, `"timezone":"Asia/Kolkata",`)
	q := testutil.Single(testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", -1, -1)))
	wantStart := time.Date(2026, 7, 26, 18, 30, 0, 0, time.UTC) // Jul 27 00:00 IST
	wantEnd := time.Date(2026, 7, 27, 18, 30, 0, 0, time.UTC)   // Jul 28 00:00 IST

	if r := testutil.GenSQL(t, c, q); !reflect.DeepEqual(r.Args, []any{wantStart, wantEnd}) {
		t.Errorf("sql args = %v, want [%s %s]", r.Args, wantStart, wantEnd)
	}
	if got := testutil.FilterJSON(t, testutil.GenMongo(t, c, q)); got != `{"createdAt":{"$gte":"2026-07-26T18:30:00Z","$lt":"2026-07-27T18:30:00Z"}}` {
		t.Errorf("mongo filter = %s", got)
	}
	esq, _ := json.Marshal(genES(t, c, q).Query)
	if !strings.Contains(string(esq), `"gte":"2026-07-26T18:30:00Z","lt":"2026-07-27T18:30:00Z"`) {
		t.Errorf("es query = %s", esq)
	}
}

// TestRelativeDateUnaffectedByTimezone guards the other half of the contract:
// a rolling relative_date is one instant, identical in every zone.
func TestRelativeDateUnaffectedByTimezone(t *testing.T) {
	q := testutil.Single(testutil.Comp("createdAt", OpAfter, testutil.VRel("hour", -3)))
	utcArgs := testutil.GenSQL(t, periodConfig(t, ""), q).Args
	istArgs := testutil.GenSQL(t, periodConfig(t, `"timezone":"Asia/Kolkata",`), q).Args
	if !reflect.DeepEqual(utcArgs, istArgs) {
		t.Errorf("relative_date moved with the timezone: UTC %v vs IST %v", utcArgs, istArgs)
	}
}

// TestPeriodUnvalidatedASTFailsInEveryGenerator is adversarial: the exported
// generators accept an AST that never saw Validate, so each must refuse a
// malformed period itself instead of emitting an empty or reversed range.
func TestPeriodUnvalidatedASTFailsInEveryGenerator(t *testing.T) {
	c := periodConfig(t, "")
	gens := map[string]Generator{"sql": SQLGenerator{}, "mysql": MySQLGenerator{}, "mongo": MongoGenerator{}, "elasticsearch": ESGenerator{}}
	for name, v := range map[string]*Value{
		"reversed":     testutil.VPeriod("day", -1, -30),
		"unknown unit": testutil.VPeriod("fortnight", -1, -1),
		"huge offset":  testutil.VPeriod("day", -1<<40, 0),
	} {
		for backend, g := range gens {
			t.Run(name+"/"+backend, func(t *testing.T) {
				q := testutil.Single(testutil.Comp("createdAt", OpBetween, v))
				if r, err := g.Generate(q, c, GenOptions{Now: testutil.FixedNow}); err == nil {
					t.Errorf("generated %+v from a malformed period; want an error", r)
				}
			})
		}
	}
}

// TestPeriodDateRouting checks a period drives Elasticsearch date-partition
// routing, and that the exclusive end does not pull in the next partition.
func TestPeriodDateRouting(t *testing.T) {
	c := testutil.MustParse(t, dateRoutingJSON) // MONTH granularity, orders-{yyyy-MM}
	cases := []struct {
		name string
		v    *Value
		want []string
	}{
		// Last month ends at exactly Jul 1 00:00Z; without the one-nanosecond
		// step back this would also select orders-2026-07.
		{"last month is one partition", testutil.VPeriod("month", -1, -1), []string{"orders-2026-06"}},
		{"last 30 days spans two", testutil.VPeriod("day", -29, 0), []string{"orders-2026-06", "orders-2026-07"}},
		{"this year spans twelve", testutil.VPeriod("year", 0, 0), []string{
			"orders-2026-01", "orders-2026-02", "orders-2026-03", "orders-2026-04", "orders-2026-05", "orders-2026-06",
			"orders-2026-07", "orders-2026-08", "orders-2026-09", "orders-2026-10", "orders-2026-11", "orders-2026-12"}},
		{"malformed period falls back to default", testutil.VPeriod("day", 0, -5), []string{"orders-legacy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &Query{Version: ASTVersion, Entity: "Order", Filter: testutil.Comp("createdAt", OpBetween, tc.v)}
			r, err := ESGenerator{}.Generate(q, c, GenOptions{Now: testutil.FixedNow})
			if err != nil {
				if tc.want[0] == "orders-legacy" {
					return // the query itself is refused, which is also safe
				}
				t.Fatalf("generate: %v", err)
			}
			if got := r.Doc.(*ESQuery).Index; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("index = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPeriodDateRoutingInTimezone: "today" in IST, asked at 01:30 IST on Aug 1,
// begins at 18:30Z on Jul 31 — so it must route to BOTH the July and August
// partitions, although the whole local day is in August.
func TestPeriodDateRoutingInTimezone(t *testing.T) {
	c := testutil.MustParse(t, strings.Replace(dateRoutingJSON, `"entity":"Order",`, `"entity":"Order","timezone":"Asia/Kolkata",`, 1))
	q := &Query{Version: ASTVersion, Entity: "Order", Filter: testutil.Comp("createdAt", OpBetween, testutil.VPeriod("day", 0, 0))}
	r, err := ESGenerator{}.Generate(q, c, GenOptions{Now: time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got, want := r.Doc.(*ESQuery).Index, []string{"orders-2026-07", "orders-2026-08"}; !reflect.DeepEqual(got, want) {
		t.Errorf("index = %v, want %v", got, want)
	}
}
