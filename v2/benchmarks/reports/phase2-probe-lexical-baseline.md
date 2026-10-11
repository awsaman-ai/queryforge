warning: using the deterministic hashing embedder — results are NOT semantic and must not be reported as research findings
model:    deterministic-trigram-hash@v1 (dim=256, non-semantic)
strategy: surface   corpus: 55 entries, 173 vectors
queries:  38, mean embed 1µs

!! NON-SEMANTIC PROVIDER — these numbers are a mechanism check, not a result !!

| query | expected | top-1 | top-3 | rank | top-1 hit | score | note |
|---|---|---|---|---|---|---|---|
| customer spending | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.843 | brief probe |
| money spent | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 0.585 | brief probe: pure paraphrase, no shared token |
| client location | customer.country | **yes** | yes | 1 | customer.country | 0.730 | brief probe: synonym for both halves |
| country where customer lives | customer.country | **yes** | yes | 1 | customer.country | 0.735 | brief probe: descriptive phrasing |
| more than | gt | **yes** | yes | 1 | gt | 1.000 | brief probe: operator phrase |
| recent customer | customer.created_at | no | no | miss | customer | 0.686 | brief probe: implies a date field |
| how much a customer paid | customer.total_spent | no | no | miss | customer | 0.577 | paraphrase of the description |
| purchase value | customer.total_spent | **yes** | yes | 1 | customer.total_spent | 1.000 | configured synonym, exact |
| when the account was opened | customer.created_at | no | yes | 2 | customer | 0.529 | paraphrase of "registration date" |
| signup date | customer.created_at | no | yes | 2 | order.placed_at | 0.435 | unconfigured synonym |
| what the customer is called | customer.name | no | no | miss | customer | 0.544 | descriptive |
| nation | customer.country | **yes** | yes | 1 | customer.country | 1.000 | single-word configured synonym |
| price of an item | product.price | no | yes | 3 | product | 0.535 | cross-entity, unambiguous |
| order total | order.amount | **yes** | yes | 1 | order.amount | 1.000 | configured synonym |
| buyers | customer | **yes** | yes | 1 | customer | 0.577 | configured synonym |
| clients | customer | **yes** | yes | 1 | customer | 0.772 | configured synonym |
| things we sell | product | no | no | miss | recent | 0.242 | paraphrase, no shared token |
| purchases | order | **yes** | yes | 1 | order | 0.858 | configured synonym |
| greater than | gt | **yes** | yes | 1 | gt | 1.000 | configured phrase |
| at least | gte | **yes** | yes | 1 | gte | 1.000 | configured phrase, easily confused with gt |
| below | lt | **yes** | yes | 1 | lt | 1.000 | configured phrase |
| how many | count | **yes** | yes | 1 | count | 1.000 | configured phrase |
| average | avg | **yes** | yes | 1 | avg | 1.000 | configured phrase |
| total of | sum | **yes** | yes | 1 | sum | 0.791 | near-configured phrase |
| customers who bought something | customer_orders | no | yes | 3 | customer | 0.500 | verb maps to an edge, not a field |
| placed an order | customer_orders | **yes** | yes | 1 | customer_orders | 0.686 | configured synonym |
| the item an order is for | order_product | **yes** | yes | 1 | order_product | 0.656 | descriptive |
| top 10 | top_n | **yes** | yes | 1 | top_n | 0.548 | configured phrase |
| the highest ones | top_n | no | yes | 2 | max | 0.516 | configured synonym |
| in the last six months | recent | **yes** | yes | 1 | recent | 0.739 | time window phrasing |
| India | customer.country:alias:India | **yes** | yes | 1 | customer.country:alias:India | 1.000 | exact alias |
| laptops | product.category:alias:laptops | **yes** | yes | 1 | product.category:alias:laptops | 1.000 | plural alias onto an enum value |
| cancelled accounts | customer.status:enum:suspended | no | no | miss | customer | 0.535 | HARD: no such word is configured; expected to miss |
| amount | — | — | — | — | order.amount | 1.000 | ambiguous between customer.total_spent, order.amount, product.price |
| date | — | — | — | — | order.placed_at | 0.722 | ambiguous between created_at and placed_at |
| name | — | — | — | — | customer.name | 1.000 | ambiguous between customer.name and product.name |
| status | — | — | — | — | customer.status | 1.000 | should reach customer.status |
| value | — | — | — | — | order.amount | 0.674 | vague; recorded to see what it attracts |

**Top-1: 23/33 (70%)  ·  Top-3: 28/33 (85%)**

### Misses (10)

- `cancelled accounts` → expected `customer.status:enum:suspended`, not retrieved
    1. customer (0.535) — account
    2. customer.status (0.378) — account status
    3. how_many (0.316) — count
- `customers who bought something` → expected `customer_orders`, found at rank 3
    1. customer (0.500) — customer
    2. order.customer_id (0.475) — The customer who placed the order.
    3. customer_orders (0.456) — customer orders
- `how much a customer paid` → expected `customer.total_spent`, not retrieved
    1. customer (0.577) — customer
    2. customer.id (0.554) — customer id
    3. order.customer_id (0.554) — customer id
- `price of an item` → expected `product.price`, found at rank 3
    1. product (0.535) — item
    2. order_product (0.535) — item
    3. product.price (0.478) — price
- `recent customer` → expected `customer.created_at`, not retrieved
    1. customer (0.686) — customer
    2. customer.id (0.585) — customer id
    3. order.customer_id (0.585) — customer id
- `signup date` → expected `customer.created_at`, found at rank 2
    1. order.placed_at (0.435) — order date
    2. customer.created_at (0.402) — joined date
    3. customer.status (0.270) — state
- `the highest ones` → expected `top_n`, found at rank 2
    1. max (0.516) — highest
    2. top_n (0.516) — highest
    3. product.name (0.357) — The name of the product.
- `things we sell` → expected `product`, not retrieved
    1. recent (0.242) — in the last
    2. product.price (0.242) — The price of a single unit of the product.
    3. product.id (0.226) — Unique identifier of the product.
- `what the customer is called` → expected `customer.name`, not retrieved
    1. customer (0.544) — customer
    2. customer.id (0.522) — customer id
    3. order.customer_id (0.522) — customer id
- `when the account was opened` → expected `customer.created_at`, found at rank 2
    1. customer (0.529) — account
    2. customer.created_at (0.379) — Date the customer account was created.
    3. customer.status (0.374) — account status
