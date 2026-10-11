## Phase 4 ranking probe — testdata/queries/phase4-commerce.tsv

- config: `testdata/configs/commerce.json`
- model: `sentence-transformers/all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41 (dim=384, semantic)`
- policy: minScore 0.70, minMargin 0.10

| sentence | span | expected | raw top-1 | sim | raw | threshold-only | ranked | decision | margin | ranked |
|---|---|---|---|---|---|---|---|---|---|---|
| client location | location | customer.country | customer.country | 1.000 | correct | correct | customer.country | accept | 0.480 | correct |
| recent customer | recent | recent | recent | 1.000 | correct | correct | recent | accept | 0.515 | correct |
| placed an order | placed | customer_orders | customer_orders | 1.000 | correct | correct | customer_orders | accept | 0.119 | correct |
| customers who bought something | bought | customer_orders | customer_orders | 1.000 | correct | correct | customer_orders | accept | 0.246 | correct |
| the highest ones | highest | AMBIGUOUS | max | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| the 10 highest customers | highest | top_n | max | 1.000 | WRONG | WRONG | top_n | accept | 0.300 | correct |
| name | name | AMBIGUOUS | customer.name | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| date | date | AMBIGUOUS | order.placed_at | 0.726 | WRONG | WRONG | — | ambiguous | 0.015 | correct |
| status | status | customer.status | customer.status | 1.000 | correct | correct | customer.status | accept | 0.581 | correct |
| value | value | AMBIGUOUS | customer.total_spent | 0.682 | WRONG | refused | — | reject | 0.000 | refused |
| amount | amount | order.amount | order.amount | 1.000 | correct | correct | order.amount | accept | 0.374 | correct |
| order amount | order amount | order.amount | order.amount | 1.000 | correct | correct | order.amount | accept | 0.525 | correct |
| customer spending | spending | customer.total_spent | customer.total_spent | 1.000 | correct | correct | customer.total_spent | accept | 0.645 | correct |
| money spent | money spent | customer.total_spent | customer.total_spent | 0.814 | correct | correct | customer.total_spent | accept | 0.223 | correct |
| more than | more than | gt | gt | 1.000 | correct | correct | gt | accept | 0.404 | correct |
| customers with total spent more than 50000 | more than | gt | gt | 1.000 | correct | correct | gt | accept | 0.404 | correct |
| customers from India | india | customer.country:alias:<redacted> | customer.country:alias:<redacted> | 1.000 | correct | correct | customer.country:alias:<redacted> | accept | 0.554 | correct |
| how many customers | how many | count | count | 1.000 | correct | correct | count | accept | 0.376 | correct |

| method | correct | refused (safe) | silently wrong |
|---|---|---|---|
| raw vector top-1 | 13/18 (72%) | 0 | 5 |
| threshold only | 13/18 (72%) | 1 | 4 |
| Phase 4 ranking | 17/18 (94%) | 1 | 0 |

RANKPROBE raw=13 threshold=13 ranked=17 scored=18 raw_wrong=5 threshold_wrong=4 ranked_wrong=0
