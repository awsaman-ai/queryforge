model:    sentence-transformers/all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41 (dim=384, semantic)
strategy: passage   corpus: 55 entries, 55 vectors
queries:  38, mean embed 2.58ms

| query | expected | top-1 | top-3 | rank | top-1 hit | score | note |
|---|---|---|---|---|---|---|---|
| customer spending | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.641 | brief probe |
| money spent | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.348 | brief probe: pure paraphrase, no shared token |
| client location | customer.country | **yes** | yes | 1 | customer.country | 0.475 | brief probe: synonym for both halves |
| country where customer lives | customer.country | **yes** | yes | 1 | customer.country | 0.789 | brief probe: descriptive phrasing |
| more than | gt | **yes** | yes | 1 | gt | 0.313 | brief probe: operator phrase |
| recent customer | customer.created_at | no | no | miss | customer.name | 0.471 | brief probe: implies a date field |
| how much a customer paid | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.505 | paraphrase of the description |
| purchase value | customer.total_spent | no | no | miss | customer.country:alias:United States | 0.493 | configured synonym, exact |
| when the account was opened | customer.created_at | **yes** | yes | 1 | customer.created_at | 0.505 | paraphrase of "registration date" |
| signup date | customer.created_at | **yes** | yes | 1 | customer.created_at | 0.444 | unconfigured synonym |
| what the customer is called | customer.name | no | yes | 2 | customer | 0.697 | descriptive |
| nation | customer.country | **yes** | yes | 1 | customer.country | 0.366 | single-word configured synonym |
| price of an item | product.price | **yes** | yes | 1 | product.price | 0.540 | cross-entity, unambiguous |
| order total | order.amount | **yes** | yes | 1 | order.amount | 0.566 | configured synonym |
| buyers | customer | **yes** | yes | 1 | customer | 0.449 | configured synonym |
| clients | customer | **yes** | yes | 1 | customer | 0.573 | configured synonym |
| things we sell | product | **yes** | yes | 1 | product | 0.442 | paraphrase, no shared token |
| purchases | order | no | yes | 2 | product | 0.436 | configured synonym |
| greater than | gt | **yes** | yes | 1 | gt | 0.574 | configured phrase |
| at least | gte | no | yes | 3 | in | 0.144 | configured phrase, easily confused with gt |
| below | lt | **yes** | yes | 1 | lt | 0.443 | configured phrase |
| how many | count | **yes** | yes | 1 | count | 0.314 | configured phrase |
| average | avg | **yes** | yes | 1 | avg | 0.579 | configured phrase |
| total of | sum | **yes** | yes | 1 | sum | 0.473 | near-configured phrase |
| customers who bought something | customer_orders | no | yes | 2 | customer | 0.666 | verb maps to an edge, not a field |
| placed an order | customer_orders | **yes** | yes | 1 | customer_orders | 0.655 | configured synonym |
| the item an order is for | order_product | **yes** | yes | 1 | order_product | 0.756 | descriptive |
| top 10 | top_n | **yes** | yes | 1 | top_n | 0.487 | configured phrase |
| the highest ones | top_n | no | yes | 2 | max | 0.630 | configured synonym |
| in the last six months | recent | **yes** | yes | 1 | recent | 0.339 | time window phrasing |
| India | customer.country:alias:India | **yes** | yes | 1 | customer.country:alias:India | 0.546 | exact alias |
| laptops | product.category:alias:laptops | **yes** | yes | 1 | product.category:alias:laptops | 0.660 | plural alias onto an enum value |
| cancelled accounts | customer.status:enum:suspended | **yes** | yes | 1 | customer.status:enum:suspended | 0.282 | HARD: no such word is configured; expected to miss |
| amount | — | — | — | — | order.amount | 0.311 | ambiguous between customer.total_spent, order.amount, product.price |
| date | — | — | — | — | order.placed_at | 0.438 | ambiguous between created_at and placed_at |
| name | — | — | — | — | customer.name | 0.343 | ambiguous between customer.name and product.name |
| status | — | — | — | — | customer.status:enum:inactive | 0.580 | should reach customer.status |
| value | — | — | — | — | product.category:enum:phone | 0.453 | vague; recorded to see what it attracts |

**Top-1: 26/33 (79%)  ·  Top-3: 31/33 (94%)**

### Misses (7)

- `at least` → expected `gte`, found at rank 3
    1. in (0.144) — in
    2. lt (0.119) — lt
    3. gte (0.079) — gte
- `customers who bought something` → expected `customer_orders`, found at rank 2
    1. customer (0.666) — customer
    2. customer_orders (0.578) — customer to order
    3. customer.country:alias:United Kingdom (0.484) — customer.country value
- `purchase value` → expected `customer.total_spent`, not retrieved
    1. customer.country:alias:United States (0.493) — customer.country value
    2. customer.country:alias:United Kingdom (0.490) — customer.country value
    3. customer.country:alias:India (0.443) — customer.country value
- `purchases` → expected `order`, found at rank 2
    1. product (0.436) — product
    2. order (0.416) — order
    3. order_product (0.402) — order to product
- `recent customer` → expected `customer.created_at`, not retrieved
    1. customer.name (0.471) — customer name
    2. recent (0.467) — recent
    3. customer.status:enum:active (0.463) — customer.status value
- `the highest ones` → expected `top_n`, found at rank 2
    1. max (0.630) — max
    2. top_n (0.423) — top_n
    3. bottom_n (0.359) — bottom_n
- `what the customer is called` → expected `customer.name`, found at rank 2
    1. customer (0.697) — customer
    2. customer.name (0.636) — customer name
    3. customer_orders (0.614) — customer to order
