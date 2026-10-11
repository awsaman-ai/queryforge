model:    sentence-transformers/all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41 (dim=384, semantic)
strategy: surface   corpus: 55 entries, 173 vectors
queries:  38, mean embed 2.707ms

| query | expected | top-1 | top-3 | rank | top-1 hit | score | note |
|---|---|---|---|---|---|---|---|
| customer spending | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.925 | brief probe |
| money spent | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.814 | brief probe: pure paraphrase, no shared token |
| client location | customer.country | no | yes | 3 | customer | 0.658 | brief probe: synonym for both halves |
| country where customer lives | customer.country | **yes** | yes | 1 | customer.country | 0.893 | brief probe: descriptive phrasing |
| more than | gt | **yes** | yes | 1 | gt | 1.000 | brief probe: operator phrase |
| recent customer | customer.created_at | no | no | miss | customer | 0.696 | brief probe: implies a date field |
| how much a customer paid | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.694 | paraphrase of the description |
| purchase value | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 1.000 | configured synonym, exact |
| when the account was opened | customer.created_at | **yes** | yes | 1 | customer.created_at | 0.780 | paraphrase of "registration date" |
| signup date | customer.created_at | **yes** | yes | 1 | customer.created_at | 0.683 | unconfigured synonym |
| what the customer is called | customer.name | **yes** | yes | 1 | customer.name | 0.775 | descriptive |
| nation | customer.country | **yes** | yes | 1 | customer.country | 1.000 | single-word configured synonym |
| price of an item | product.price | **yes** | yes | 1 | product.price | 0.806 | cross-entity, unambiguous |
| order total | order.amount | **yes** | yes | 1 | order.amount | 1.000 | configured synonym |
| buyers | customer | **yes** | yes | 1 | customer | 0.880 | configured synonym |
| clients | customer | **yes** | yes | 1 | customer | 0.889 | configured synonym |
| things we sell | product | **yes** | yes | 1 | product | 0.608 | paraphrase, no shared token |
| purchases | order | **yes** | yes | 1 | order | 0.861 | configured synonym |
| greater than | gt | **yes** | yes | 1 | gt | 1.000 | configured phrase |
| at least | gte | **yes** | yes | 1 | gte | 1.000 | configured phrase, easily confused with gt |
| below | lt | **yes** | yes | 1 | lt | 1.000 | configured phrase |
| how many | count | **yes** | yes | 1 | count | 1.000 | configured phrase |
| average | avg | **yes** | yes | 1 | avg | 1.000 | configured phrase |
| total of | sum | **yes** | yes | 1 | sum | 0.868 | near-configured phrase |
| customers who bought something | customer_orders | no | no | 5 | order | 0.703 | verb maps to an edge, not a field |
| placed an order | customer_orders | no | no | 4 | order | 0.783 | configured synonym |
| the item an order is for | order_product | **yes** | yes | 1 | order_product | 0.840 | descriptive |
| top 10 | top_n | **yes** | yes | 1 | top_n | 0.653 | configured phrase |
| the highest ones | top_n | no | yes | 2 | max | 0.797 | configured synonym |
| in the last six months | recent | **yes** | yes | 1 | recent | 0.615 | time window phrasing |
| India | customer.country:alias:India | **yes** | yes | 1 | customer.country:alias:India | 1.000 | exact alias |
| laptops | product.category:alias:laptops | **yes** | yes | 1 | product.category:alias:laptops | 1.000 | plural alias onto an enum value |
| cancelled accounts | customer.status:enum:suspended | no | yes | 3 | customer | 0.586 | HARD: no such word is configured; expected to miss |
| amount | — | — | — | — | order.amount | 1.000 | ambiguous between customer.total_spent, order.amount, product.price |
| date | — | — | — | — | order.placed_at | 0.726 | ambiguous between created_at and placed_at |
| name | — | — | — | — | customer.name | 1.000 | ambiguous between customer.name and product.name |
| status | — | — | — | — | customer.status | 1.000 | should reach customer.status |
| value | — | — | — | — | customer.total_spent | 0.682 | vague; recorded to see what it attracts |

**Top-1: 27/33 (82%)  ·  Top-3: 30/33 (91%)**

### Misses (6)

- `cancelled accounts` → expected `customer.status:enum:suspended`, found at rank 3
    1. customer (0.586) — account
    2. customer.status (0.518) — account status
    3. customer.status:enum:suspended (0.428) — <redacted>
- `client location` → expected `customer.country`, found at rank 3
    1. customer (0.658) — client
    2. customer.name (0.621) — client name
    3. customer.country (0.548) — location
- `customers who bought something` → expected `customer_orders`, found at rank 5
    1. order (0.703) — A single purchase placed by a customer.
    2. customer.total_spent (0.698) — customer spend
    3. customer (0.685) — customer
- `placed an order` → expected `customer_orders`, found at rank 4
    1. order (0.783) — order
    2. order.placed_at (0.781) — Date the order was placed.
    3. order.customer_id (0.741) — The customer who placed the order.
- `recent customer` → expected `customer.created_at`, not retrieved
    1. customer (0.696) — customer
    2. recent (0.685) — recent
    3. customer.name (0.661) — customer name
- `the highest ones` → expected `top_n`, found at rank 2
    1. max (0.797) — highest
    2. top_n (0.797) — highest
    3. min (0.577) — lowest
