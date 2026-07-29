package queryforge

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// canonicalAST is the running example from the design doc (§7). It exercises
// every Value kind: enum, boolean, relative_date and array.
const canonicalAST = `{
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

// TestQueryRoundTrip is the happy path: parse -> serialize -> parse must yield
// an identical tree. Comparing parsed-vs-reparsed (not against the raw string)
// is robust to JSON numeric normalization.
func TestQueryRoundTrip(t *testing.T) {
	var q1 Query
	if err := json.Unmarshal([]byte(canonicalAST), &q1); err != nil {
		t.Fatalf("initial unmarshal: %v", err)
	}
	raw, err := json.Marshal(q1)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var q2 Query
	if err := json.Unmarshal(raw, &q2); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if !reflect.DeepEqual(q1, q2) {
		t.Errorf("round trip changed the tree:\n first=%+v\n second=%+v", q1, q2)
	}
}

// TestBooleanFalseSurvives is the adversarial case that motivated the custom
// Value marshaller: a naive `omitempty` on the payload would silently drop a
// boolean false. This guards against that regression.
func TestBooleanFalseSurvives(t *testing.T) {
	v := Value{Kind: KindBoolean, V: false}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"v":false`) {
		t.Errorf("boolean false was dropped, got %s", raw)
	}
	var back Value
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b, ok := back.AsBool(); !ok || b != false {
		t.Errorf("round-tripped bool wrong: ok=%v v=%v", ok, b)
	}
}

// TestRelativeDateShape checks that relative_date serializes with unit+amount
// and no "v", and that a negative amount (30 days ago) survives.
func TestRelativeDateShape(t *testing.T) {
	v := Value{Kind: KindRelativeDate, Unit: "day", Amount: -30}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, `"v"`) {
		t.Errorf("relative_date should not carry v, got %s", s)
	}
	if !strings.Contains(s, `"amount":-30`) || !strings.Contains(s, `"unit":"day"`) {
		t.Errorf("relative_date lost unit/amount, got %s", s)
	}
	var back Value
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Amount != -30 || back.Unit != "day" {
		t.Errorf("relative_date round trip wrong: %+v", back)
	}
}

// TestValueNullPayload feeds an explicit null payload; V must come back nil,
// not a panic or a typed zero.
func TestValueNullPayload(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`{"kind":"string","v":null}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.V != nil {
		t.Errorf("expected nil payload, got %#v", v.V)
	}
	if _, ok := v.AsString(); ok {
		t.Errorf("AsString should fail on nil payload")
	}
}

// TestValueAccessorsWrongType is adversarial: the typed accessors must return
// ok=false when the payload is a different type, never panic.
func TestValueAccessorsWrongType(t *testing.T) {
	v := Value{Kind: KindString, V: "hello"}
	if _, ok := v.AsFloat(); ok {
		t.Errorf("AsFloat on a string should be !ok")
	}
	if _, ok := v.AsBool(); ok {
		t.Errorf("AsBool on a string should be !ok")
	}
	if _, ok := v.AsSlice(); ok {
		t.Errorf("AsSlice on a string should be !ok")
	}
	if s, ok := v.AsString(); !ok || s != "hello" {
		t.Errorf("AsString failed: ok=%v s=%q", ok, s)
	}
}

// TestSelectProjectionRoundTrips ensures the new projection field survives
// (de)serialization and is omitted when empty.
func TestSelectProjectionRoundTrips(t *testing.T) {
	q := NewQuery("Order")
	q.Select = []string{"status", "createdAt"}
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"select":["status","createdAt"]`) {
		t.Errorf("select not serialized, got %s", raw)
	}

	empty := NewQuery("Order")
	rawEmpty, _ := json.Marshal(empty)
	if strings.Contains(string(rawEmpty), "select") {
		t.Errorf("empty select should be omitted, got %s", rawEmpty)
	}
}

// TestMalformedJSONRejected is the worst-case parse: garbage in must error out,
// not silently produce a half-built tree.
func TestMalformedJSONRejected(t *testing.T) {
	var q Query
	if err := json.Unmarshal([]byte(`{"entity": "Order", "filter": {`), &q); err == nil {
		t.Errorf("expected error on truncated JSON")
	}
}

// TestReadOnlyInvariant is the GET-only guarantee expressed as a test: the
// operator catalogue must never contain a mutating verb, and its size is
// pinned so an accidental addition trips this test.
func TestReadOnlyInvariant(t *testing.T) {
	forbidden := []string{"insert", "update", "delete", "drop", "set", "create", "truncate", "alter", "write", "upsert"}
	for _, op := range AllOperators {
		low := strings.ToLower(string(op))
		for _, bad := range forbidden {
			if low == bad {
				t.Errorf("mutating operator %q leaked into the read-only catalogue", op)
			}
		}
	}
	if len(AllOperators) != 19 {
		t.Errorf("operator catalogue size changed to %d; confirm the new operator is read-only and update this pin", len(AllOperators))
	}
}
