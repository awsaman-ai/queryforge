# 022 — Phase 8 gate decisions

**Status:** accepted — Phase 8 **closed** at the final gate (2026-09-23) ·
**Phase:** 8 · **Date:** 2026-09-22, extended 2026-09-23

At the Phase 8 gate the user kept the phase **open** until four items were
addressed, and set the rule for each (§1–§4). At the re-gate they approved the
rule §1's measurement had found but not built, and asked for the one remaining
wrong case to be investigated: §5 and §6, added 2026-09-23. At the final gate
(2026-09-23) they accepted the phase and decided the one case the investigation
left open (§7). This records how each was built, what it changed, and what it
costs. Decision 021 stands; this extends it.

Two principles run through all four, both the user's:

- **Generic, never domain-specific.** Every rule works for any configured
  schema. None names a phrase, a field or an SQL keyword.
- **Similarity discovers; deterministic rules decide.** The model proposes
  candidates. Only the config and the sentence's own words decide whether a
  candidate is allowed.

## §1 — Semantic completeness: a qualifier is never silently discarded

**The findings.** `show product discount price` and `show product list price`
both compiled to `product.price` (Phase 8 P10, P11). Two different mechanisms
produced them:

- **"discount"** was accepted as product.price on similarity plus entity
  context (0.561 + 0.15 = 0.711), as its own span, beside "price". Two spans
  read the same target, so the word contributed nothing to the query.
- **"list"** is a Phase 3 function word (a request verb, as in "list
  customers"), so nothing downstream ever saw it.

The exploration set (70 sentences of the shape, with controls) found four more
silent wrongs of the same two kinds: `show discount price`, `show list price of
products`, `show products with discount price above 100` and `products with
list price below 50`.

**The rule (the user's), made exact.** The rule runs in binding
(`internal/binding/complete.go`), after ranking's verdict and before any
structure is built. It only refuses; it never changes a reading.

- **A phrase** is a run of cover spans that touch (no token between them) and
  whose readings agree: the same target, or a value and its own field. So
  "discount" and "price", both read as product.price, are one phrase.
- When a phrase contains a **complete configured surface** of its target
  (every word of one of its names, synonyms, phrases or values), the phrase is
  accepted only when:
  - its content words are exactly **one** such surface's words ("product
    price", "unit price"), or
  - every other content word has **another valid role the reading keeps**:
    - the entity that owns the target ("customer amount spent");
    - an endpoint of a relationship target ("placed orders");
    - for a value, the value's own field ("country India").
- Otherwise the sentence is refused: **UNSUPPORTED_QUERY**, naming the word's
  position, the phrase and the target, and why. The config has no target for
  the whole phrase.
- A word from **another** of the same target's surfaces is not a second role:
  - "cost price" mixes the synonym "cost" and the name "price" into a phrase
    the config does not have;
  - "total amount" borrows "total" from "order total" while meaning a sum.
- **Request verbs** (show, list, give, get, find, display, fetch, return,
  select) count as content words, except:
  - the sentence's opening request verb (the first one, with only function
    words before it);
  - one followed directly by a function word ("give me a list of customers").

  A request verb in content position is explained only by a configured phrase
  of its reading that contains it, literally: 021 §2's standard for write
  keywords. Ranking treated it as a function word, so ranking accepting its
  span says nothing about it ("return date" is accepted as placed_at on "date"
  alone).

**Not affected, deliberately:**

- **Paraphrase** with no complete surface: "money spent" (the brief's probe).
  There a word *replaces* a configured word; it does not qualify a target
  already named. Where the configured word is shared, 021 §6 already rejects it.
- **A word with its own reading**: "average price".

**Alternatives measured:**

| Variant | Result | Why not |
|---|---|---|
| Accept any word from any of the target's surfaces | missed `show total amount` read as one span (order.amount has "order total") | mixes two configured phrases into one the author did not write; the user's rule asks for the complete phrase |
| No phrase grouping (one span at a time) | misses P10 itself: "discount" alone contains no configured word | the finding's first mechanism is two spans |
| Refuse every span with an unconfigured word, complete surface or not | refuses "money spent" and every paraphrase (mutation M21 fails 3 existing pipeline tests) | undoes Phase 2; decision 021 §6 measured the same shape |

**The cost.**

- `show product cost price` is now refused. It compiled to price, and "cost
  price" is a different accounting quantity.
- A request verb at the end of a sentence has no function word after it, so it
  counts as content: `show customers list` and `show price list` are refused.
- `can you show customers` was already refused, for the unexplained "you"; it
  is now refused for the request verb in content position.

**Known limit.** `list price of products` compiles to price. The opening
request verb is read as the request ("list the price of products"). English
allows both readings, and the rule takes the command one.

**G33 and G34 were scoped by the user at the re-gate and are now §5.** The
paragraphs below record how they were found.

**Found while measuring §1: G33, G34**

- `products with price and discount above 10` compiled to price > 10;
- `price of discount` compiled to price.

"Discount" is again accepted on similarity as a target the sentence already
names by its configured word, but in a *separate* phrase (a connective or a
function word between), so §1's phrase rule does not see it.

A generic fix was measured and reverted: a phrase read by similarity alone
(no configured word of its target) that duplicates the **same** target
another phrase names by a configured word is refused. It refuses both. It
changes nothing in the Phase 7 set (77), the rest of the Phase 8 set, the 70
exploration sentences or the Phase 4 probes.

A wider form, which also counts a field and its own value as the same target,
refused a correct Phase 7 answer: V03 `clients located in India`.

## §2 — Numeric precision: exact from the sentence to the parameters

**The finding (V02).** `customers who spent more than 99999999999999999999999`
sent 1e23. Numbers were float64 at every step.

**The representation.** `internal/numeric.Decimal` is an exact base-10
number: sign, digits, scale, kept canonical. It uses no floating point and
has no third-party dependency.

- **The zero value is not a number.** A hand-built plan value without one is
  refused by the compiler, not read as 0.
- **The range is PostgreSQL numeric's** (131072 digits before the point, 16383
  after). The bound is checked before any digits are built. A mutation that
  removed that check still refused every out-of-range value, but only after
  building its digits: 2 GB for a shift of 2^31−1, and 10 MB for the 9-byte
  JSON literal `1e9999999`. A test now measures the allocation.

**Carried through every stage:**

- **Extraction:** `nlp.Number.Value`.
- **Binding:** `binding.Value.Number`. Its explanation used `%.6f` and showed
  0.1234567 as 0.123457; it now shows every digit.
- **SemanticPlan:** `plan.Value.Number`. The JSON form is the exact literal,
  and `{"number": 1e23}` is read as exactly 10^23.
- **Compiler:** the contradiction check (M06) compares digits. float64 called
  `= 99999999999999999999999 AND = 99999999999999999999998` redundant.
- **Parameters:** `Driver()` gives int64 for a whole number in int64's range
  and the exact decimal text otherwise, never float64. A driver sends text,
  and PostgreSQL parses it into the column's numeric type exactly.
  - **This changes the public contract.** Through `api.ToSQL` and
    POST /v1/noLLM/tosql, a fractional or huge number arrives as a JSON string
    (`"0.1234567890123456789"`); an int64 arrives as a JSON number, as before.
  - Whole numbers are int64 across int64's whole range; before, only up to
    2^53.
- **Ranking and binding count tests** (`isCount`): the same decisions as the
  float test for every integer in int64's range.

**Silent value changes found while making it exact.** All are fixed, and each
is pinned by a test:

| Input | Before | Now |
|---|---|---|
| `more than -5` | 5 (the sign was punctuation) | −5 |
| `more than .5` | 5 | 0.5 |
| `more than 1,5` / `1.000,5` | 15 / 1.0005 (commas stripped) | refused: only en-US grouping |
| `more than 12x` / `30th` | 12 / 30 (the tag was ignored) | refused |
| `customers from India 1.2.3` | the token was dropped and the query compiled | refused |
| `more than 1e309` | the token was dropped ("gt has no value") | refused, for its real reason |
| `one billion billion` | 2000000000 | refused: the scale words must descend |
| `one thousand two million` | 2001000 (summed), and 1002 in this round's first fix (found by mutation) | refused |
| `4.35k` | 4350 via float (correct by luck of rounding) | 4350 exactly |
| `in the last 2.5 months` / `0 days` | 2 months / 1 day | no window forms, so refused |
| `top 9007199254740993 customers` | LIMIT 9007199254740992 | LIMIT 9007199254740993 |
| `in the last 2147483647 days` | compiled, with a start instant in year −5877584 (no valid timestamp) | refused: the period leaves years 1–9999 |

**The accepted notation.** Digits; en-US thousands grouping; one decimal
point; then optionally a k, m, mm, bn, b or t magnitude or a currency code.
Or number words whose scale words descend. A minus sign or decimal point
written against the number belongs to it. A hyphen glued to a word does not
("top-5"). Anything else is `Malformed`, reported by extraction and refused
by binding (VALUE_RESOLUTION_FAILED), never approximated.

**The cost.** Exponent notation (`1e5`) is refused. Float parsing accepted it
before, by accident. Ordinals (`30th`) are refused.

## §3 — Multi-statement input: the input boundary

**The finding (I07).** `customers from India; select * from orders` compiled
to customers from India *with* orders: two requests merged into one reading.

**The rule** (`internal/inputguard`) is generic syntax, not vocabulary. It
never searches for a keyword. A request is one query, so it refuses:

- the statement separator `;`;
- the SQL comment delimiters `--`, `/*` and `*/`.

The refusal is SECURITY_REJECTED at the **input stage**, before extraction.
It names the delimiter from a closed list and its position, and never quotes
the request.

- **What it scans.** The request after `nlp.Normalize`, the text every stage
  reads, so a zero-width character cannot split `--`. Each character whose
  Unicode compatibility form (NFKC) is one of the four delimiter characters
  also counts. There are nine, derived from Unicode 16.0 data, not chosen by
  hand: fullwidth and small forms, and U+037E, which NFC itself maps to ";".
  A test pins the table.
- **Why refuse what could not reach the SQL.** Values are parameters,
  identifiers come from the config, and the output guard checks the text, so
  none of these can end a statement or comment one out. They are refused for
  what they mean in the *request*: "a second request starts here", or "ignore
  what follows". The pipeline reads every word, so it would do neither.
- **Not matched:**
  - `#`: an operator in PostgreSQL, and "number" in English;
  - a single `-`, `/` or `*`;
  - spaced forms (`- -`).
- **Verified to produce no query and reach nothing:**
  - `Run` returns no reading, plan or query, and the retriever (the model) is
    never called (`TestStatementSyntaxNeverReachesAStage`, which also covers
    the diagnostic path);
  - `api.ToSQL` returns a coded error and an empty Result;
  - live `POST /v1/noLLM/tosql` (go_test_qf, unmodified) returns HTTP 400
    with the code and no `query`;
  - `FuzzPipeline` holds, as an invariant on arbitrary input, that a request
    containing a delimiter never passes the input stage.

**The cost.** A `;` anywhere in a request is refused, including inside a
value (`named Smith;Jones`). So is `--` typed as a dash. The message says why.

## §4 — maxPhrases = 256, approved

The user approved 256 as the default (021 §1's measurement). It is documented
at `config.DefaultSecurityPolicy`. The resource bounds are pinned by these
tests, all retained and run by the gate (section 22):

- **Input length:** `TestMaxInputLengthInBothDirections` (new). No test
  covered it in both directions. It counts characters, not bytes: at the
  limit the request is read; one past it is refused at input, never
  truncated, and never reaches retrieval.
- **maxPhrases:** `TestMaxPhrasesComesFromTheConfig` and
  `TestDefaultPhraseLimitIsPhase3sLimit`.
- **maxCandidates:** `TestCandidateDepthIsCheckedAtStartup` and
  `TestCandidatesPerSpanAreRefusedNotTruncated`.
- **Nesting depth:** `TestNestingDepthLimit` and
  `TestNestingCheckRefusesUnknownPredicates`.


## §5 — An independent span may not resolve by similarity to a target the sentence names

**The user's rule, at the re-gate:** if the sentence already explicitly names a
configured target, another independent span must not resolve to that same
target using similarity alone.

- `products with price and discount above 10` → refused (was `price > 10`).
- `price of discount` → refused (was `SELECT price`).

It runs in binding beside §1 (`explicitTargets`), and like §1 it can only
refuse. §1 covers a phrase — spans that touch; §5 covers the rest of the
sentence, where a connective or a function word separates the two readings.

**The three terms, all generic:**

| Term | Means | Read from |
|---|---|---|
| explicitly names | a phrase holds a content word of one of its target's configured names, synonyms, phrases or values | the config's own vocabulary; a description is prose and never counts (decision 019) |
| by similarity alone | the phrase holds no such word: retrieval and context proposed the target, and nothing written names it | the sentence's words |
| the same target | the same corpus entry | the target's identity |

**A value and its own field are NOT the same target.** Phase 7's V03 `clients
located in India` reads "located" as customer.country by similarity while
"India" is one of that field's values — a correct answer. The wider form,
which treats them as one target, refuses it. Measured, and rejected.

**Paraphrase with its own configured evidence is untouched**, which is what
"alone" carries: "customers with amount spent over 100 and total spent below
50" names total_spent twice, and both phrases hold "spent". A paraphrase of a
target named nowhere else is untouched too ("customers registered last year").

**The refusal** names both spans and the target, and always the FIRST explicit
mention, so the message does not depend on how often the sentence names it.

**Measured cost: none.** The rule changes nothing in the Phase 7 set (77
cases), the rest of the Phase 8 set, the 70-sentence exploration, an
18-sentence paraphrase stress list, or the Phase 4 probes and sweep. The only
changed outcomes are G33 and G34.

## §6 — The config, not the position, decides the opening request verb

**The investigation the user asked for: `list price of products`.**

It is **a separate issue from semantic completeness**, not another shape of
§1 or §5. In §1 and §5 a word is discarded — no role in the reading. Here the
opening request verb HAS a role, the one English gives it: it is the request.
"list price of products" reads as "list the price of products", which is why
it compiles to product.price, and "list customers from India" reads the same
way. What the engine cannot tell from position alone is whether the words
after it are a compound the config knows ("list price").

**Measured first.** With a config that has a `list_price` field whose synonym
is "list price", the configured field won in every phrasing tried — 18
sentences over two configs, sentence-initial, after another request verb
("get list price"), through a condition, an aggregate and a value filter. No
phrasing silently fell back to product.price. So the engine already resolved
the ambiguity correctly wherever the config had an opinion.

**The rule** makes that a guarantee rather than a property of ranking's
preferences: when a configured surface of more than one word RUNS from the
verb, word for word ("list price…"), the verb is not the request frame. It
becomes content, and §1's standard then applies: the reading must name that
target literally, or the sentence is refused — never answered from a reading
that dropped the word.

- **Contiguity is required.** The looser test — every word of the surface
  somewhere in the sentence, as decision 019 §5 uses when choosing between
  targets — refused `list products by price`, an ordinary command, because
  "list" and "price" both appear in it. A compound is contiguous, as §1's
  phrases are.
- **More than one word.** A one-word surface that happens to be a request verb
  is that word alone, not a phrase the verb opens.
- **No config opinion, no change.** commerce configures no phrase holding
  "list", so `list price of products` still compiles to product.price, and
  every ordinary command is untouched.

**Measured cost: none.** No outcome changed in any corpus.

**The residual risk, stated plainly.** A user who means a term the config does
not have ("list price", where the config has only "price") gets the command
reading and a query for price. The engine has no evidence to do otherwise: the
words name no other configured target. The remedy is the config's, as it is
for 021 §6's "signup date" — configure the term, and §6 guarantees it wins.
The asymmetry with `show list price of products`, which IS refused, is
deliberate: there the request slot is already filled, so the verb can only be
a qualifier, and no configured phrase explains it.

## §7 — G36 `list price of products` compiles: intentional, not a defect

**The user's decision at the final gate (2026-09-23).** Keep `list price of
products` compiling as it does. Do **not** add another refusal rule for it.
Record it as intentional behaviour — a known semantic interpretation — rather
than as a bug or a deferred finding.

**The reasoning given.** `list` functions as the request verb when `list price`
is not a configured semantic target; a config that does define `list price` can
cause that configured target to resolve instead. Both halves are
configuration-driven and domain-agnostic, which is what §6 already guarantees:
a configured surface of two or more words running contiguously from the verb
takes the verb out of the request frame, and the reading must then name that
target literally.

**What this settles.** The investigation in §6 ended with one accepted query a
reviewer would want to look at twice (Phase 8 corpus case G36,
`list price of products` → `SELECT "t0"."price"`). Round 3 argued it is not a
silently wrong accepted query — commerce has no list-price concept, so no
configured target is discarded and no word the config could explain is ignored.
The user has now decided that reading is the intended one. G36 therefore stays
an expected `sql` case in `testdata/queries/phase8-adversarial.json`, and the
acceptance criterion — **zero silently wrong accepted queries** — holds with it
counted as correct, not excused.

**What is NOT being done.** No new rule, no phrase-specific special case, and
no change to §6. The remedy for a deployment that means a term its config
lacks is to configure the term, exactly as for 021 §6's "signup date".

**What would reopen it.** A Phase 10 corpus where a request verb opening a
*configured* multi-word surface is read as a command anyway (that would be a
§6 defect, not this decision), or measured evidence that real users write
`list price` meaning a list-price concept often enough that refusing the
unconfigured case is cheaper than reading it as a command.

## Changes to earlier artefacts

- **Phase 7 query set** (the user asked for changed outcomes to be updated):
  - Z04 `' OR 1=1 --`: NO_SEMANTIC_MATCH → SECURITY_REJECTED (§3).
  - Z05: expected `sql` (a known failure, refused as DROP) → expects and gets
    SECURITY_REJECTED (§3).
  - Nothing else changed. **71/77**, and all 49 accepted queries are
    byte-identical.
- **Phase 4:** both probe reports are byte-identical, and the calibration
  sweep is identical in all 45 cells — re-measured again after §5 and §6, which
  are binding rules and cannot move a ranking cell.
- **The Phase 8 set** grew to 93 cases: 57 original, G01–G32 for §1–§4, G33 and
  G34 (§5) and G35, G36 (§5's control and §6's command reading).
- **Phase 3 behaviour** (numbers only, §2): the inputs in §2's table. Phase 3's
  own tests pass unchanged in meaning; they now compare exact values.
- **Phase 6 plan goldens:** unchanged.
- **Closed records:** `phase-0…7-results.md` and decisions 001–020 are
  unchanged, at the final gate too.
- **The final gate (2026-09-23):** passed on the evidence in
  `phase-8-results.md` ("Final gate"). No code changed for §7 — it is a
  decision about an existing, measured behaviour.

## What would cause us to revisit it

- A Phase 10 corpus where §5's shape appears in legitimate sentences (one
  target named explicitly and paraphrased again in the same sentence, with no
  configured word in the paraphrase).
- A config whose multi-word surface opens with a request verb in a way §6's
  contiguity test misreads.
- A config whose phrases legitimately mix two of one target's surfaces (§1's
  "cost price" cost at scale), or a Phase 10 corpus where request verbs in
  content position are common.
- A locale requirement (decimal commas), which §2 refuses today.
