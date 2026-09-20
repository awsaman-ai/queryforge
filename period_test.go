package queryforge

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// periodEngineConfig is an Asia/Kolkata orders config with a date field that
// allows between, so calendar periods are in the model's vocabulary.
func periodEngineConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(`{
	  "entity": "Order",
	  "timezone": "Asia/Kolkata",
	  "model": {"provider":"openai","model":"x"},
	  "backends": {"sql": {"table": "orders"}, "mongo": {"collection": "orders"}},
	  "defaults": {"limit": 50, "maxLimit": 500},
	  "fields": [
	    {"name":"status","type":"enum","values":["PLACED","DELIVERED"]},
	    {"name":"createdAt","type":"date","operators":["before","after","between"]}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return c
}

// TestTranslatePeriodEndToEnd runs "orders from yesterday" through the public
// engine: the prompt the model sees, the compiled SQL and Mongo, and the
// readback all agree on the Asia/Kolkata calendar day, from one pinned clock.
func TestTranslatePeriodEndToEnd(t *testing.T) {
	reply := `{"filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"day","from":-1,"to":-1}}}`
	// 2026-09-15 20:00Z is already 01:30 on Sep 16 in Kolkata, so "yesterday"
	// is Sep 15 IST — [Sep 14 18:30Z, Sep 15 18:30Z). A UTC implementation
	// would answer Sep 14 instead.
	now := time.Date(2026, 9, 15, 20, 0, 0, 0, time.UTC)
	wantStart := time.Date(2026, 9, 14, 18, 30, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 15, 18, 30, 0, 0, time.UTC)

	p := &clockProvider{}
	e := NewWithProvider(periodEngineConfig(t), &recordingReply{clockProvider: p, reply: reply})
	e.Now = func() time.Time { return now }

	res, err := e.Translate(context.Background(), "orders from yesterday", "sql", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(p.system, "Today (Asia/Kolkata): 2026-09-16") {
		t.Errorf("prompt did not carry the local date")
	}
	if want := "SELECT * FROM orders WHERE (createdAt >= $1 AND createdAt < $2) LIMIT 50"; res.Query.SQL != want {
		t.Errorf("sql = %s, want %s", res.Query.SQL, want)
	}
	if !reflect.DeepEqual(res.Query.Args, []any{wantStart, wantEnd}) {
		t.Errorf("args = %v, want [%s %s]", res.Query.Args, wantStart, wantEnd)
	}
	if !strings.Contains(res.Explain, "createdAt falls in yesterday (Asia/Kolkata time)") {
		t.Errorf("explain = %q", res.Explain)
	}
	if res.RepairAttempts != 0 {
		t.Errorf("a well-formed period needed %d repairs", res.RepairAttempts)
	}

	mres, err := e.Translate(context.Background(), "orders from yesterday", "mongo", nil)
	if err != nil {
		t.Fatalf("translate mongo: %v", err)
	}
	if got := tFilter(t, mres.Query); got != `{"createdAt":{"$gte":"2026-09-14T18:30:00Z","$lt":"2026-09-15T18:30:00Z"}}` {
		t.Errorf("mongo filter = %s", got)
	}
}

// TestTranslatePeriodRepairs is adversarial: the model's first reply is a
// period with "to" missing (would have silently meant yesterday AND today),
// its second reverses the bounds (would have matched nothing). Both must be
// refused and repaired, and the final query must be the corrected one.
func TestTranslatePeriodRepairs(t *testing.T) {
	missingTo := `{"filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"day","from":-1}}}`
	reversed := `{"filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"day","from":-1,"to":-30}}}`
	good := `{"filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"day","from":-30,"to":-1}}}`

	c := periodEngineConfig(t)
	e := NewWithProvider(c, &scriptedProvider{responses: []string{missingTo, reversed, good}})
	e.Now = func() time.Time { return time.Date(2026, 9, 15, 20, 0, 0, 0, time.UTC) }

	res, err := e.Translate(context.Background(), "orders in the 30 days before today", "sql", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if res.RepairAttempts != 2 || len(res.Repairs) != 2 {
		t.Fatalf("repairs = %d (%+v), want 2", res.RepairAttempts, res.Repairs)
	}
	if !strings.Contains(res.Repairs[0].Message, `"from" and "to"`) {
		t.Errorf("first repair should name the missing bound: %+v", res.Repairs[0])
	}
	if !strings.Contains(res.Repairs[1].Message, "is after to") {
		t.Errorf("second repair should name the reversed bounds: %+v", res.Repairs[1])
	}
	wantStart := time.Date(2026, 8, 16, 18, 30, 0, 0, time.UTC) // Aug 17 00:00 IST
	if got := res.Query.Args[0]; got != wantStart {
		t.Errorf("start = %v, want %v", got, wantStart)
	}
}

// recordingReply wraps clockProvider so the test can both capture the system
// prompt and answer with a real AST.
type recordingReply struct {
	*clockProvider
	reply string
}

func (r *recordingReply) Complete(ctx context.Context, system, user string) (string, error) {
	_, _ = r.clockProvider.Complete(ctx, system, user)
	return r.reply, nil
}
