## Phase 4 ranking probe — testdata/queries/phase4-ambiguity.tsv

- config: `testdata/configs/ambiguity.json`
- model: `sentence-transformers/all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41 (dim=384, semantic)`
- policy: minScore 0.70, minMargin 0.10

| sentence | span | expected | raw top-1 | sim | raw | threshold-only | ranked | decision | margin | ranked |
|---|---|---|---|---|---|---|---|---|---|---|
| amount | amount | AMBIGUOUS | order.amount | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| value | value | AMBIGUOUS | payment.amount | 0.616 | WRONG | refused | — | reject | 0.000 | refused |
| date | date | AMBIGUOUS | order.created_at | 0.726 | WRONG | WRONG | — | ambiguous | 0.005 | correct |
| status | status | AMBIGUOUS | customer.status | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| name | name | AMBIGUOUS | customer.name | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| show the amount | amount | AMBIGUOUS | order.amount | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| list every status | status | AMBIGUOUS | customer.status | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| highest | highest | AMBIGUOUS | max | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| orders with amount over 100 | amount | order.amount | order.amount | 1.000 | correct | correct | order.amount | accept | 0.200 | correct |
| payments with amount over 100 | amount | payment.amount | order.amount | 1.000 | WRONG | WRONG | payment.amount | accept | 0.200 | correct |
| the amount of the payment | amount | payment.amount | order.amount | 1.000 | WRONG | WRONG | payment.amount | accept | 0.200 | correct |
| orders by date | date | order.created_at | order.created_at | 0.726 | correct | correct | order.created_at | accept | 0.205 | correct |
| payment date | payment date | payment.paid_at | payment.paid_at | 1.000 | correct | correct | payment.paid_at | accept | 0.534 | correct |
| status of the payment | status | payment.status | customer.status | 1.000 | WRONG | WRONG | payment.status | accept | 0.200 | correct |
| customers with status zorbified | status | customer.status | customer.status | 1.000 | correct | correct | customer.status | accept | 0.200 | correct |
| product name | product name | product.name | product.name | 1.000 | correct | correct | product.name | accept | 0.529 | correct |
| order amount | order amount | order.amount | order.amount | 1.000 | correct | correct | order.amount | accept | 0.543 | correct |
| payment amount | payment amount | payment.amount | payment.amount | 1.000 | correct | correct | payment.amount | accept | 0.363 | correct |
| names of customers | names | customer.name | customer.name | 0.812 | correct | correct | customer.name | accept | 0.200 | correct |
| the 5 highest orders | highest | top_n | max | 1.000 | WRONG | WRONG | top_n | accept | 0.300 | correct |
| top 5 payments | top | top_n | top_n | 0.825 | correct | correct | top_n | accept | 0.206 | correct |
| maximum order amount | maximum | max | max | 1.000 | correct | correct | max | accept | 0.383 | correct |
| how many customers | how many | count | count | 1.000 | correct | correct | count | accept | 0.376 | correct |
| cost of a product | cost | product.price | product.price | 1.000 | correct | correct | product.price | accept | 0.710 | correct |
| remittances over 50 | remittances | payment | payment | 0.978 | correct | correct | payment | accept | 0.442 | correct |
| clients | clients | customer | customer | 0.889 | correct | correct | customer | accept | 0.297 | correct |
| customers and products by name | name | AMBIGUOUS | customer.name | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| orders and payments with amount over 5 | amount | AMBIGUOUS | order.amount | 1.000 | WRONG | WRONG | — | ambiguous | 0.000 | correct |
| payments over 500 | over | gt | gt | 1.000 | correct | correct | gt | accept | 0.557 | correct |
| payments of at least 500 | at least | gte | gte | 1.000 | correct | correct | gte | accept | 0.755 | correct |
| customers from Freedonia | freedonia | customer.country:alias:<redacted> | customer.country:alias:<redacted> | 1.000 | correct | correct | customer.country:alias:<redacted> | accept | 0.774 | correct |
| payments that bounced | bounced | payment.status:alias:<redacted> | payment.status:alias:<redacted> | 1.000 | correct | correct | payment.status:alias:<redacted> | accept | 0.797 | correct |
| customers who placed an order | placed | customer_orders | customer_orders | 1.000 | correct | correct | customer_orders | accept | 0.452 | correct |
| customers who placed an order | order | order | order | 1.000 | correct | correct | order | accept | 0.135 | correct |
| what is the highest payment amount | highest | — | max | 1.000 | — | — | — | ambiguous | 0.000 | — |

| method | correct | refused (safe) | silently wrong |
|---|---|---|---|
| raw vector top-1 | 20/34 (59%) | 0 | 14 |
| threshold only | 20/34 (59%) | 1 | 13 |
| Phase 4 ranking | 33/34 (97%) | 1 | 0 |

RANKPROBE raw=20 threshold=20 ranked=33 scored=34 raw_wrong=14 threshold_wrong=13 ranked_wrong=0
