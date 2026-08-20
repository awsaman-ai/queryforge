package queryforge

import "testing"

// TestElasticConfigRejections covers every load-time guardrail specific to
// Elasticsearch/OpenSearch source configuration: mutually exclusive source
// modes, the required routing default, routing-field eligibility, pattern/
// date token shape, rule ambiguity, if/else branch shape, index-name safety,
// and the keywordMapping/nestedPath restrictions on Field.
func TestElasticConfigRejections(t *testing.T) {
	field := func(extra string) string {
		return `{"entity":"Order","model":{},"backends":{"elasticsearch":{` + extra + `}},` +
			`"fields":[{"name":"tenantId","type":"string","routingField":true}]}`
	}
	cases := map[string]string{
		"index + alias both set":         field(`"index":"orders","alias":"orders-alias"`),
		"index + indexes both set":       field(`"index":"orders","indexes":["orders-2025","orders-2026"]`),
		"invalid index name (uppercase)": field(`"index":"Orders"`),
		"invalid alias name":             field(`"alias":"Orders-Alias"`),
		"duplicate indexes":              field(`"indexes":["orders-a","orders-a"]`),
		"invalid entry in indexes":       field(`"indexes":["orders-a","Orders-B"]`),

		"routing with no default": field(`"routing":{"strategy":"pattern","field":"tenantId","indexPattern":"tenant-{tenantId}-orders"}`),
		"routing default has invalid index name": field(`"routing":{"strategy":"pattern","field":"tenantId",` +
			`"indexPattern":"tenant-{tenantId}-orders","default":["Bad-Name"]}`),
		"routing field not registered": field(`"routing":{"strategy":"pattern","field":"nope",` +
			`"indexPattern":"tenant-{x}-orders","default":["orders-default"]}`),

		"pattern without indexPattern token": field(`"routing":{"strategy":"pattern","field":"tenantId",` +
			`"indexPattern":"tenant-orders","default":["orders-default"]}`),
		"pattern with two tokens": field(`"routing":{"strategy":"pattern","field":"tenantId",` +
			`"indexPattern":"tenant-{a}-{b}-orders","default":["orders-default"]}`),
		"pattern literal has invalid characters": field(`"routing":{"strategy":"pattern","field":"tenantId",` +
			`"indexPattern":"Tenant-{tenantId}-orders","default":["orders-default"]}`),

		"unknown strategy": field(`"routing":{"strategy":"bogus","field":"tenantId",` +
			`"indexPattern":"tenant-{tenantId}-orders","default":["orders-default"]}`),
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(js)); err == nil {
				t.Errorf("expected rejection, got none")
			}
		})
	}
}

// TestElasticDateRoutingRejections covers the "date" strategy's own checks:
// field type, granularity/token correspondence.
func TestElasticDateRoutingRejections(t *testing.T) {
	base := func(fieldType, routing string) string {
		return `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":` + routing + `}},` +
			`"fields":[{"name":"createdAt","type":"` + fieldType + `","routingField":true}]}`
	}
	cases := map[string]string{
		"date routing on non-date field": base("string",
			`{"strategy":"date","field":"createdAt","granularity":"YEAR","indexPattern":"orders-{yyyy}","default":["orders-legacy"]}`),
		"granularity/token mismatch": base("date",
			`{"strategy":"date","field":"createdAt","granularity":"YEAR","indexPattern":"orders-{yyyy-MM}","default":["orders-legacy"]}`),
		"invalid granularity": base("date",
			`{"strategy":"date","field":"createdAt","granularity":"DECADE","indexPattern":"orders-{yyyy}","default":["orders-legacy"]}`),
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(js)); err == nil {
				t.Errorf("expected rejection, got none")
			}
		})
	}
}

// TestElasticRulesRejections covers "rules"/"ifElse" shape checks: operator
// vocabulary, type-checked values, duplicate conditions, and branch ordering.
func TestElasticRulesRejections(t *testing.T) {
	rulesCfg := func(rules string) string {
		return `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":` +
			`{"strategy":"rules","default":["orders-global"],"rules":` + rules + `}}},` +
			`"fields":[{"name":"region","type":"string","routingField":true},` +
			`{"name":"amount","type":"number","routingField":true}]}`
	}
	ifElseCfg := func(branches string) string {
		return `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":` +
			`{"strategy":"ifElse","default":["orders-legacy"],"branches":` + branches + `}}},` +
			`"fields":[{"name":"region","type":"string","routingField":true}]}`
	}
	cases := map[string]string{
		"rule field not routing-eligible": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":` +
			`{"strategy":"rules","default":["orders-global"],"rules":[{"when":{"field":"region","operator":"equals","value":"US"},"indexes":["orders-us"]}]}}},` +
			`"fields":[{"name":"region","type":"string"}]}`,
		"rule uses contains operator": rulesCfg(`[{"when":{"field":"region","operator":"contains","value":"US"},"indexes":["orders-us"]}]`),
		"rule gt on string field":     rulesCfg(`[{"when":{"field":"region","operator":"gt","value":"US"},"indexes":["orders-us"]}]`),
		"rule value not a number":     rulesCfg(`[{"when":{"field":"amount","operator":"gt","value":"not-a-number"},"indexes":["orders-big"]}]`),
		"rule has no indexes":         rulesCfg(`[{"when":{"field":"region","operator":"equals","value":"US"},"indexes":[]}]`),
		"duplicate rule at same priority": rulesCfg(`[
			{"when":{"field":"region","operator":"equals","value":"US"},"indexes":["orders-us"]},
			{"when":{"field":"region","operator":"equals","value":"US"},"indexes":["orders-us-2"]}
		]`),

		"ifElse branch with neither if nor else": ifElseCfg(`[{"indexes":["orders-x"]}]`),
		"ifElse branch with both if and else":    ifElseCfg(`[{"if":{"field":"region","operator":"equals","value":"US"},"else":true,"indexes":["orders-x"]}]`),
		"ifElse else not last": ifElseCfg(`[
			{"else":true,"indexes":["orders-legacy"]},
			{"if":{"field":"region","operator":"equals","value":"US"},"indexes":["orders-us"]}
		]`),
		"ifElse no branches": ifElseCfg(`[]`),
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(js)); err == nil {
				t.Errorf("expected rejection, got none")
			}
		})
	}
}

// TestFieldElasticRestrictions covers keywordMapping and nestedPath: the
// type restriction on the former, and the containment requirement on the
// latter — the same shape of check ValueCase and Mongo's ElemMatch already
// enforce, kept for the Elasticsearch/OpenSearch-only fields.
func TestFieldElasticRestrictions(t *testing.T) {
	cases := map[string]string{
		"keywordMapping on a number field": `{"entity":"Order","model":{},"fields":[
			{"name":"amount","type":"number","keywordMapping":{"elasticsearch":"amount.keyword"}}
		]}`,
		"nestedPath with no elastic mapping inside it": `{"entity":"Order","model":{},"fields":[
			{"name":"sku","type":"string","nestedPath":"items"}
		]}`,
		"nestedPath mapping does not sit inside it": `{"entity":"Order","model":{},"fields":[
			{"name":"sku","type":"string","nestedPath":"items","mapping":{"elasticsearch":"lineItems.sku"}}
		]}`,
		"invalid nestedPath shape": `{"entity":"Order","model":{},"fields":[
			{"name":"sku","type":"string","nestedPath":"items..bad","mapping":{"elasticsearch":"items.sku"}}
		]}`,
		"invalid keywordMapping path shape": `{"entity":"Order","model":{},"fields":[
			{"name":"name","type":"string","keywordMapping":{"elasticsearch":"name..keyword"}}
		]}`,
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(js)); err == nil {
				t.Errorf("expected rejection, got none")
			}
		})
	}
}

// TestElasticConfigValid is the happy path for every source mode and every
// routing strategy, checked once so a false positive in the rejection tests
// above cannot hide behind "everything gets rejected".
func TestElasticConfigValid(t *testing.T) {
	cases := map[string]string{
		"direct index": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"index":"orders"}},
			"fields":[{"name":"status","type":"string"}]}`,
		"alias": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"alias":"orders-alias"}},
			"fields":[{"name":"status","type":"string"}]}`,
		"multiple index": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"indexes":["orders-2025","orders-2026"]}},
			"fields":[{"name":"status","type":"string"}]}`,
		"pattern routing": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":
			{"strategy":"pattern","field":"tenantId","indexPattern":"tenant-{tenantId}-orders","default":["orders-default"]}}},
			"fields":[{"name":"tenantId","type":"string","routingField":true}]}`,
		"date routing": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":
			{"strategy":"date","field":"createdAt","granularity":"MONTH","indexPattern":"orders-{yyyy-MM}","default":["orders-legacy"]}}},
			"fields":[{"name":"createdAt","type":"date","routingField":true}]}`,
		"rules routing": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":{
			"strategy":"rules","default":["orders-global"],
			"rules":[
				{"when":{"field":"region","operator":"equals","value":"US"},"indexes":["orders-us"],"priority":1},
				{"when":{"field":"region","operator":"equals","value":"EU"},"indexes":["orders-eu"],"priority":1}
			]}}},
			"fields":[{"name":"region","type":"string","routingField":true}]}`,
		"ifElse routing": `{"entity":"Order","model":{},"backends":{"elasticsearch":{"routing":{
			"strategy":"ifElse","default":["orders-legacy"],
			"branches":[
				{"if":{"field":"createdAt","operator":"gte","value":"2026-01-01"},"indexes":["orders-2026"]},
				{"else":true,"indexes":["orders-legacy"]}
			]}}},
			"fields":[{"name":"createdAt","type":"date","routingField":true}]}`,
		"keywordMapping and nestedPath together": `{"entity":"Order","model":{},"fields":[
			{"name":"sku","type":"string","nestedPath":"items","mapping":{"elasticsearch":"items.sku"},
			 "keywordMapping":{"elasticsearch":"items.sku.keyword"}}
		]}`,
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(js)); err != nil {
				t.Errorf("expected this config to parse clean, got: %v", err)
			}
		})
	}
}
