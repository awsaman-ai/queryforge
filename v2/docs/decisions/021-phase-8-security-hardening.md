# 021 — Phase 8: security, ambiguity and hardening

**Status:** proposed; at the gate the user kept Phase 8 open until four items were addressed, which [decision 022](022-phase-8-gate-decisions.md) records (awaiting the final gate) · **Phase:** 8 · **Date:** 2026-09-22

Section 16 of the brief. Three choices were the user's, made before any code
was written: how read-only requests are rejected (§2), what `maxCandidates`
bounds (§1), and whether the "update date" finding is Phase 8 work (§6). The
rest are recorded here so they can be reviewed at the gate.

Phase 7 stays closed: none of this reopens its ranking or binding to change a
Phase 7 result, and X11 stays deferred exactly as decision 020 records. The one
change to a Phase 7 artefact (§9) was approved in advance and is listed case by
case.

## §1 — What each section 16.7 limit counts

Before Phase 8, four limits were enforced (input length, joins per path,
filters, query length). Three were declared and never read (`maxPhrases`,
`maxCandidates`, `maxPlanningMillis`), and there was no nesting limit.

| Limit | Counts | Enforced | Code |
|---|---|---|---|
| `maxInputLength` 512 | characters of the request | pipeline, before anything runs | INVALID_INPUT |
| `maxPhrases` **256** (was 64) | candidate spans extraction produced | pipeline, before retrieval | INVALID_INPUT |
| `maxCandidates` 256 | candidates for **one** span | `pipeline.New` (declared depth) and per request | CONFIG_ERROR / SECURITY_REJECTED |
| `maxJoins` 4 | tables joined to the root, **whole query** | graph (per path, as before) and compiler | SECURITY_REJECTED |
| `maxFilters` 16 | WHERE + HAVING conditions | binding and compiler (unchanged) | SECURITY_REJECTED |
| `maxNestingDepth` **1** (new) | subquery levels | compiler | SECURITY_REJECTED |
| `maxQueryLength` 8192 | bytes of the compiled SQL | compiler (unchanged) | SECURITY_REJECTED |
| `maxPlanningMillis` 2000 | wall time of rank → bind → plan → compile | pipeline, at each stage boundary | SECURITY_REJECTED |

- **`maxPhrases` counts candidate spans, and its default rose from 64 to 256.**
  Spans are about four per word (decision 010), so 64 would refuse ordinary
  sentences over ~17 words; the brief's example 1 already produces 38. 256 is
  the limit Phase 3 has applied all along (`nlp.DefaultOptions.MaxSpans`), so
  enforcing the config value changed no earlier result. The config value
  replaces the hardcoded one inside the pipeline. A test pins the two defaults
  together.
- **`maxCandidates` bounds one span** (the user's choice). A declared depth
  (the CLI's `--top-k`) over it fails at startup. A retriever that returns more
  than the limit for a span is refused per request, never truncated: dropping
  the tail would change which runner-up the margin is measured against. The
  request total is then bounded by `maxPhrases × maxCandidates`. It never
  refuses a sentence of the declared scope. As a request total, the default
  would have refused the brief's example 1 (350 candidates).
- **`maxJoins` counts the union of the query's paths:** every distinct path
  prefix from the root, once. The per-path bound in the graph stays. The union
  is at least the longest path, so this is the stricter, query-wide bound.
  - *Alternative measured and rejected:* counting the tables the lowered SQL
    reads. Lowering re-reads a to-many path inside each EXISTS or COUNT
    subquery to prevent fan-out (decision 015). That is an artefact of how the
    query is made safe, not something the request asked for. The per-read
    count refused the reviewed Phase 6 golden `exists-and-count` (4 reads
    against commerce's limit of 3; its paths reach 2 tables).
- **`maxNestingDepth` is a new config key, default 1.** The grammar makes one
  level of correlated subquery and never nests them, so 1 refuses nothing
  today. It is measured on the lowered tree, and an unknown predicate kind is
  refused as INTERNAL_ERROR rather than counted as flat.
- **`maxPlanningMillis` covers the deterministic stages only,** measured from
  the end of retrieval. Embedding time is the worker's, and Phase 9 measures
  it. It is checked at each stage boundary. The other limits bound each stage,
  so the overshoot is at most one stage. A request over the limit is refused
  even if it would have finished. This is the one refusal that depends on the
  machine rather than on the sentence: at 2000 ms against stage times under
  10 ms, it is a safety net.

## §2 — Read-only rejection (16.1): a keyword needs the config (the user's choice)

The engine was already read-only by construction: the grammar has one
statement, SELECT. Write requests were refused as unexplained words
(UNSUPPORTED_QUERY). They are now refused explicitly: **SECURITY_REJECTED, "the
word at tokens[i:j] is the write keyword DELETE; the engine only reads".**

- **The keywords** are the eleven SQL statements section 16.1 names, plus
  MERGE: INSERT, UPDATE, DELETE, DROP, ALTER, TRUNCATE, CREATE, GRANT, REVOKE,
  COPY, CALL, MERGE. It is a closed list of SQL vocabulary, not English.
  Matching uses ranking's light fold (case, one plural s), so "deletes"
  matches. "deleted", "created" and "updating" do not: inflected forms are
  ordinary read vocabulary ("customers created in 2024" still compiles).
  English write verbs that are not SQL keywords ("remove") are not on the
  list; they stay refused as unexplained words.
- **Only a configured phrase explains a keyword.** The token must lie in an
  accepted span, and that span's accepted target must have a configured name,
  synonym or phrase that contains the word, all of whose words are in the
  sentence. A field with the synonym "update date", in "customers by update
  date", is a read.
  - Similarity is not an explanation. An ambiguous span, a description, a
    partial phrase, or an accepted span elsewhere in the sentence explains
    nothing. This is the same line decision 019 draws for values.
- **It runs before ranking's verdict** (`pipeline.Admit`: the read-only rule,
  then the verdict). So "SELECT * FROM users; DROP TABLE users;" is refused as
  a write, not as "matches nothing". (Since decision 022 §3 its ";" refuses it
  one stage earlier, at the input boundary; without the separator it is still
  refused as a write.)
  - `Run` applies it. `--rank` shows the reading and then the refusal.
    `--bind` and `--plan` refuse before binding, so no diagnostic plans a
    sentence `--query` refuses.
  - The message names the keyword from the list and a token position, and
    never quotes the sentence (decision 013).

## §3 — An output guard over the finished SQL

Everything before the compiler's last step keeps the output safe by
construction. Phase 8 adds a check that does not trust that: **a lexer over the
rendered text that accepts only the grammar's closed lexicon.** It allows:

- the fixed keywords;
- double-quoted configured tables and columns, and generated aliases `tN`;
- `$1…$n` in order, one per parameter;
- integers (LIMIT, `SELECT 1`);
- fixed punctuation and operators;
- the single literal `'\'` after ESCAPE;
- one statement, starting with SELECT and ending in its only `;`.

Anything else is SECURITY_REJECTED, with a byte offset and a category, never
the offending text. A guard failure can only mean a compiler bug; the guard
makes such a bug a refusal, never a query. "Never unsafe SQL" is now a checked
property of every query the engine returns.

## §4 — The config validator's warning channel (decisions 016 and 019 §4)

`internal/configlint` reports warnings. They never reject a config.
`--validate-config` prints them (exit 0), and in JSON too. Requests stay quiet
(section 24). It is a separate package because both rules need the corpus's
surfaces, ranking's own definition of which readings agree, and Phase 3's own
parser; importing them into `config` would create a cycle, and copying them
would drift. `ranking` gained two thin exported wrappers (`Agrees`,
`FoldWords`) with no change in behaviour.

- **`phrase_shared_across_kinds` (019 §4).** A phrase configured on targets of
  different semantic kinds that ranking does not treat as one reading.
  - *Exact* ("product" on an entity and a relationship; "highest" on max and
    top_n) and *contained* ("placed" on a relationship, inside placed_at's
    "placed at"). Readings that agree are not reported ("how many" on count
    and the count pattern).
  - Two exemptions for the contained shape:
    - a phrase naming the entity that owns the longer phrase's target ("order"
      in "order date"), or the entity a join key references ("customer" in
      order.customer_id's "customer id");
    - a word inside a config value, which is matched only in full (019 §1).
  - Measured on commerce.json: 13 contained collisions before the exemptions
    (6 join-key names and 1 word inside a value), 6 after.
- **`phrase_never_forms` (016).** A configured phrase in which Phase 3's own
  parser (`nlp.Analyze` on the phrase) finds a number or a date, so it can
  never match as written. "one of" and "not one of" contain "one". The date
  case is the same defect found by the same check: "last year" and "this
  month" on the recent pattern.
- **Warnings, not errors.** Neither shape is provably unusable. Ranking
  refuses a shared phrase the sentence does not settle, and the target of a
  phrase that never forms stays reachable through its other phrases. As
  errors, they would stop commerce.json from loading and break every earlier
  gate.
- **Redaction.** A phrase that is a config value is redacted by default, and a
  value target is named by its field. The first draft quoted a phrase shared
  by a value and a synonym from the synonym's side, leaking the value; this
  was found while writing the test, and a mutation that reverts the fix is
  caught.

## §5 — Local embedding privacy (16.11): offline by default

**Finding.** Measured with a Python audit hook, the embedding worker contacted
huggingface.co on every start (a DNS lookup and a TLS connection to port 443),
although the weights were cached. No query text was sent. The worker README
said the only network access was the first download; that was not true.

**Fix.** The worker forces `HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1`,
overriding any caller setting. The first download needs
`QF_V2_ALLOW_MODEL_DOWNLOAD=1`. Offline, the revision is read from the local
cache's snapshot directory. Without that, it would come back as `unknown`,
and every cached index, whose identity includes it, would look stale. It still
resolves to `1110a243…`, so every report header is unchanged.

`embedding_worker/netcheck.py` runs the worker under the audit hook. The Go
test requires zero socket connections and DNS lookups through a full
identity + embed + shutdown exchange. It first runs the hook's self-test, which
must see a loopback connection: a check that cannot see a connection cannot
vouch for the absence of one.

## §6 — A generic word qualified by an unconfigured word (16.8; finding "update date"; the user's choice)

**Finding.** `show customers by update date` compiled to `SELECT created_at`.
"update date" was accepted as created_at on entity context (similarity 0.575,
+0.15 because the sentence names customer), with a 0.369 margin. The runner-up
placed_at was penalised because "date" alone carries it. The measured list
found the same shape in `show customers by modification date` (created_at),
`orders by shipping date`, `show order shipping date` and `orders by order
shipping date` (all placed_at). Two things let it through:

- 019 §5's rule only looks at spans ALL of whose words are in the reading's
  configured phrases, and "update" is in none of them.
- Binding's unexplained-word rule (016) never saw "update", because it was
  inside an accepted span.

**The rule.** A word that is a strict fragment of several targets' configured
phrases ("date" in "joined date" and "order date") is shown by the config to be
generic: it needs a qualifier to name one of them. If a span qualifies it with
a content word that none of the reading's configured phrases contains, the span
names a target the config does not have. It is **rejected as a reading,
whatever the sentence spells out elsewhere.** The cover then reads its words
separately:

- the generic word meets §5 on its own (refused, or settled by spelled-out
  evidence);
- the qualifier meets 016 (an unexplained word refuses the sentence), or its
  own ranking.

Each refusal the user sees states its own, true reason.

**Scope, deliberately narrow.** Only a shared (generic) word triggers it.
"money spent" is untouched: "spent" has one home, and "money" is paraphrase
(the brief's probe). A span with no configured word ("registered") is semantic
linking and untouched. A function word set aside does not trigger it.

**Alternatives measured** on the Phase 4 probes (34 + 18 rows), the Phase 7
query set (77), and 33 sentences of the shape with controls, diffed before and
after. A further 14 sentences (a qualifier on a word with one home: "net
spending", "order net amount") compared the built rule with the broad variant.

| Variant | Probes | Query set | Shape list | Why not |
|---|---|---|---|---|
| Similarity floor: no acceptance below MinScore | measured in 019: breaks D01–D03 | — | misses "shipping date" (similarity **0.729**, above the floor) | refuses semantic linking and misses the finding |
| §5 extended, AMBIGUOUS only (settled spans accepted) | byte-identical | byte-identical | misses 3 of the 5 silent wrongs ("orders by shipping date" is settled by "orders") | spelled-out evidence cannot turn "shipping date" into a configured phrase |
| Broad: reject any span with a configured and an unconfigured word, shared or not | byte-identical | byte-identical | identical to the built rule, on the 33 and on the 14 | nothing measured needs it, and it changes a Phase 4 layer on unmeasured input |
| **Built: reject when the configured word is shared** | **byte-identical** | **byte-identical (70/77)** | **4 silent wrongs refused, 1 cost (below); all 16 controls that compiled still compile; nothing newly compiles** | — |

**The cost:** `customers by signup date` compiled to created_at, plausibly
correctly. It is now refused, because "signup" is configured nowhere and 019 §5
says similarity is not evidence. The fix for a config that wants it is a
synonym.

**Residual risk.** Once the span is rejected, the qualifier faces ranking on
its own. If context carried it to some target, that reading would stand. With
the real model, "shipping" is ambiguous (margin 0.068) and the sentence is
refused. This is decision 011's context evidence, not this rule.

## §7 — The e2e runner: `REFUSED`, a title, visible control characters

- **`expect: "REFUSED"`:** any explicit refusal passes, and a query never
  does. It is used only where the brief's gate asks for "an explicit
  rejection" and which rule refuses first is not what the case tests. A case
  that tests a rule names its code.
- **An optional report title.** The Phase 7 set has none and keeps its
  heading.
- **Control and invisible format characters** in inputs are shown as
  `\uXXXX` in the report. A NUL byte made the first Phase 8 report a binary
  file, and a zero-width space would hide what the input held.

The Phase 7 report is byte-identical under all three.

## §8 — Findings recorded, not fixed (await scope)

**Scoped and fixed at the gate** (decision 022): P10/P11 by §1 (semantic
completeness), V02 by §2 (exact numbers), I07 by §3 (the input boundary). The
table below is the record as it stood before the gate.

Each is a safe outcome (one SELECT, the guard passed, values as parameters),
but the answer does not match the question. Each is a known case in the
Phase 8 set.

| Case | What happens | Shape | Why not fixed here |
|---|---|---|---|
| P10, P11 `show product discount/list price` | compiles to product.price | an unconfigured word inside a wholly present configured phrase ("product … price", lexical evidence 0.5): a different mechanism from §6 | widening §6 to lexical evidence changes how Phase 4 reads configured phrases, and forces re-splits with their own risks ("total money spent" could re-read "total" as SUM) |
| V02 `… more than 99999999999999999999999` | the value is sent rounded to 1e23 | numbers are float64 end to end | the fix belongs in the Phase 6 planner (refuse a literal float64 cannot hold exactly) |
| I07 `customers from India; select * from orders` | compiles to customers from India WITH orders | Phase 3 treats "select" as a function word and `;` as punctuation, so two requests merge | refusing multi-statement input is a new rule |

## §9 — Changes to earlier artefacts

- **Phase 7 query set:** Z01, Z02 and Z03 now expect SECURITY_REJECTED (they
  expected UNSUPPORTED_QUERY). Their case notes had said Phase 8 would add the
  explicit rejection; the user approved the change when choosing §2. Z05 is
  still a known failure; its category changed from UNSUPPORTED_FEATURE to
  SECURITY_REJECTION (the same refusal, now for its real reason). No accepted
  query changed, and the report is otherwise byte-identical: **70/77, 0
  silently wrong, 49/49 valid PostgreSQL.**
- **Phase 4 probes:** byte-identical (33/34, 17/18, 0 silently wrong).
- **Phase 6 plan goldens:** unchanged, and all within the new whole-query
  limits.
- `docs/phase-7-results.md` and decision 019 are unchanged. Decision 020
  remains the sole record for X11.

## What would cause us to revisit it

- A Phase 10 failure pattern in §6's refusals (the "signup date" cost at scale).
- A configured phrase containing a write keyword that the literal-explanation
  rule refuses in practice.
- `maxPlanningMillis` refusing a real request, which would mean a stage is not
  bounded the way §1 assumes.
- The user scoping any §8 finding. (Done at the gate: decision 022.)
