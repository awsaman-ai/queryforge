# Test data

## Valid configs

### `customers.json`

The baseline: one `customer` entity, four fields, three operators. Copied from
section 7 of the experiment brief **unchanged**, so a result reported against it
traces back to the specification rather than to a local edit.

| Field | Type | Exercises |
|---|---|---|
| `name` | string | plain field selection and text matching |
| `country` | string | **value aliases** — "India" in language, `IN` in storage |
| `total_spent` | number | numeric comparison, aggregation, ordering, and a per-field operator allowlist |
| `created_at` | date | absolute and relative date ranges ("last year", "last 6 months") |

It has no relationships, aggregation phrases or query patterns — exactly as the
brief prints it. That is a feature: it is the config that proves the loader
handles a minimal file, and the one whose provenance is beyond question.

### `commerce.json`

The multi-entity fixture: `customer`, `order` and `product`, 15 fields, all nine
operators, all five aggregations, two relationships and four query patterns.

It exists because several things cannot be tested with a single entity:

- **Join traversal.** `customer → order → product` is the two-hop path behind
  "customers who bought laptops", the brief's own Phase 5 probe.
- **Direction independence.** `order` is the declared `from` side of one
  relationship and the `to` side of another, which is what
  `RelationshipsTouching` has to get right.
- **Enums.** `customer.status` and `product.category` carry declared values plus
  aliases onto them ("laptops" → `laptop`), so value resolution has something
  closed to resolve against.
- **Field flags.** Join keys such as `order.customer_id` are configured
  `selectable: false, filterable: false`: reachable for a join, invisible to
  natural language.
- **Partial security config.** It sets `maxJoins` and `maxFilters` only, proving
  that overriding one limit does not discard the others.

Every field carries a `description` and `synonyms`, which is the point rather
than decoration. Embedding the bare token `total_spent` is weak, because nobody
asks for "total_spent" — they ask what someone "spent". The corpus text built in
Phase 2 is the name plus description plus synonyms plus type plus entity, and
closing that vocabulary gap is the whole reason the experiment has an embedding
layer (brief, section 7).

### `ambiguity.json`

The Phase 4 fixture, built to the brief's section 12 gate: *"construct
intentional ambiguity ... where multiple fields have similar meanings."*
`customer`, `order`, `payment` and `product`, 19 fields, three relationships.

| Word | Fields that answer to it |
|---|---|
| `amount` | `order.amount`, `payment.amount`, both named exactly that |
| `status` | `customer.status`, `order.status`, `payment.status` |
| `name` | `customer.name`, `product.name` |
| date | `customer.created_at`, `order.created_at`, `payment.paid_at` |
| `value` | "order value", "payment value" |

Its config values are deliberately **distinctive** (`Freedonia`, `zorbified`,
`settled_qx`), so the redaction sweep can never confuse a leaked value with
ordinary prose (decision 006 explains why fixtures must avoid values like
`a`/`b`). `customer_orders` spans customer → order, and `order_payments` spans
order → payment, so "payment" is two joins from "customer". Relationship
scoring needs that distance.

### `multipath.json`

The Phase 5 fixture, for the two schema-graph rejections section 13 requires:

- **Two equally plausible join paths.** `order` reaches `customer` through
  `order_billing_customer` AND `order_shipping_customer`, one join each. With
  no verb, the path is AMBIGUOUS_MATCH. "shipped to" or "billed to" picks one.
- **A disconnected entity.** `warehouse` has no relationship at all, so any
  sentence joining it to customers is RELATIONSHIP_NOT_FOUND.

Its values (`Ruritania`, `RU_QX`) are distinctive, like ambiguity.json's.

## Invalid configs

One rule per file wherever possible, so a passing test proves the validator
diagnosed *that* problem and not something it tripped over first. The rule names
are the closed `config.Rule` set; `TestGoldenFixtures` asserts the **exact** set
each file breaks, so a spurious extra violation fails the suite too.

| File | Rules |
|---|---|
| `invalid-malformed-json.json` | not valid JSON (rejected before validation) |
| `invalid-no-entities.json` | `no_entities` |
| `invalid-unnamed-entity.json` | `missing_entity_name` |
| `invalid-duplicate-entity.json` | `duplicate_entity` |
| `invalid-unknown-entity.json` | `unknown_entity` |
| `invalid-duplicate-field.json` | `duplicate_field` |
| `invalid-field-type.json` | `unsupported_field_type` |
| `invalid-operator-type.json` | `operator_type_mismatch` |
| `invalid-aggregation-type.json` | `aggregation_type_mismatch` |
| `invalid-unknown-operator.json` | `unknown_operator` |
| `invalid-identifier.json` | `invalid_identifier` |
| `invalid-value-alias.json` | `malformed_value_alias` |
| `invalid-enum.json` | `enum_values` |
| `invalid-missing-phrases.json` | `missing_phrases` |
| `invalid-relationship.json` | `unknown_relationship_target`, `invalid_cardinality` |
| `invalid-relationship-types.json` | `relationship_type_mismatch` |
| `invalid-query-pattern.json` | `duplicate_pattern`, `unknown_pattern_intent`, `unknown_entity` |
| `invalid-security-limits.json` | `invalid_limit` |

Four rules have no fixture file because they need a config that cannot be
written to disk, or that another rule masks first: `missing_field_name`,
`missing_field_entity`, `unknown_aggregation` and `duplicate_relationship` are
covered by in-memory configs in `validate_test.go`.
`TestEveryRuleHasAFixture` enforces that the list of exceptions stays honest.

Some fixtures are worth reading for what they assert about safety rather than
syntax. `invalid-identifier.json` maps a table to
`customers; DROP TABLE users --`: user text can never become an identifier, but a
config author can still write one, and the compiler trusts config identifiers by
construction. `invalid-relationship.json` includes a `many-to-many` cardinality,
which is refused with an explanation rather than quietly treated as
`one-to-many` — the latter would duplicate parent rows and return a wrong answer
with no error.

## Not here yet

Created by the phase that needs them, rather than as empty directories now:

- `queries/` — natural-language inputs (Phase 3 onward)
- `expected/` — golden semantic plans and SQL (Phase 6–7)
- `ambiguity/` — inputs with deliberately competing interpretations (Phase 4)
- `adversarial/` — injection-shaped and destructive inputs (Phase 8)

## Note on the data itself

There is no database and no rows anywhere in this experiment — only schema
descriptions. The engine generates queries; it never runs them.
