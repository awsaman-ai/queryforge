# QueryForge V2 Experiment — TODO

**Currently working on:** Phase 9 — performance, concurrency and memory
benchmarking. Measured and documented; **awaiting the manual gate**. Record:
[`docs/phase-9-results.md`](docs/phase-9-results.md),
[decision 023](docs/decisions/023-phase-9-benchmark-method.md),
[`benchmarks/reports/performance.md`](benchmarks/reports/performance.md). Phase 8 **closed** at its final gate (2026-09-23) after three
gate rounds; record: [`docs/phase-8-results.md`](docs/phase-8-results.md)
("Final gate" first), [decision 021](docs/decisions/021-phase-8-security-hardening.md),
[decision 022](docs/decisions/022-phase-8-gate-decisions.md) §1–§7.
**Phases 0–8 are closed records** — not edited again unless Phase 9 produces a
clearly scoped regression.
**Next manual checkpoint:** the Phase 9 gate — `v2/benchmarks/reports/performance.md`
plus a re-run of `scripts/run-manual-tests.sh`. Phase 10 does not start until
the user passes it.

## Status Legend
- [ ] Not started
- [~] In progress
- [x] Completed
- [!] Blocked
- [-] Intentionally deferred

## Phase 0 — Scope / Baseline
- [x] Verify current QueryForge tests pass before v2 changes
- [x] Create isolated `v2/`
- [x] Create isolated `v2/go.mod`
- [x] Create CLI contract
- [x] Add baseline config
- [x] Add baseline test fixtures
- [x] Manual gate passed (2026-09-20)

## Phase 1 — Config Model
- [x] Entity model
- [x] Field model
- [x] Operator model
- [x] Aggregation model
- [x] Relationship model
- [x] Config validator
- [x] Unit tests
- [x] Manual gate passed (2026-09-20)
- [x] FieldType, QueryPattern, SecurityPolicy models (also named in section 9)
- [-] ValueAlias as a named type — modelled as `map[string]string` on Field instead, because
      the config shape is a JSON object and a struct would need custom unmarshalling to read
      the same file. Validated by `RuleMalformedValueAlias`, resolved by
      `SchemaRegistry.ResolveValueAlias`. Revisit if aliases need per-entry metadata.
- [x] SchemaRegistry
- [x] 18 invalid fixtures, one rule each; exact-set golden test
- [x] Redaction contract: values redacted by default, revealed under --debug, enforced
      by `qferr.Sensitive` plus a generic fixture sweep (decision 006)

## Phase 2 — Semantic Corpus / Vector Index
- [x] Semantic corpus builder — 7 entry kinds, deterministic, no model needed
- [x] EmbeddingProvider interface
- [x] Local embedding implementation — Python sidecar over stdio, one process reused
- [x] Model/version pinning — MiniLM-L6-v2 @ 1110a243, recorded with every index
- [x] Vector store/index — in-memory dense, cosine, full scan
- [x] Startup caching — build once; --build-index/--index persists across restarts
- [x] Retrieval tests — determinism, dimension validation, reload, corruption, staleness
- [x] Both embedding strategies implemented and measured (decision 008)
- [x] `--probe` mode: accuracy over a query set against one warm model
- [x] Manual gate passed (2026-09-21) — 27/33 Top-1 = 82%, 30/33 Top-3 = 91%

## Phase 3 — NLP Extraction
- [x] Normalization — unicode folding, whitespace, invisible characters
- [x] Tokenization — exact byte offsets, lexical kinds, function-word marking
- [x] Phrase spans — overlapping n-grams, no segmentation (decision 010)
- [x] Number extraction — digits, grouping, k/m suffixes, currency, percent, spelled-out
- [x] Date extraction — rolling vs calendar windows, absolute, half-open resolution
- [x] Operator phrase extraction — survives as its own span for Phase 4 to bind
- [x] Conjunction/disjunction detection — and / or / not
- [x] Tests — 161 functions, 271 cases, paraphrase stability, fuzz robustness
- [x] `--analyze` CLI mode, config-free and model-free
- [x] Manual gate passed (2026-09-21)

## Phase 4 — Candidate Ranking
- [x] Semantic score — retrieval similarity kept untouched, never used as confidence
- [x] Lexical score — exact / surface-wholly-present; never negative (decision 011)
- [x] Type compatibility — attached number/date vs field type
- [x] Entity compatibility — own entity named elsewhere; symmetric for entities and fields
- [x] Operator compatibility — attached operator vs field allowlist; operator vs value type
- [x] Relationship compatibility — join distance for fields; both ends for traversals
- [x] Context score — count beside top_n / aggregation, date beside time_window
- [x] Span quality — boundary function words, swallowed values, carried by shorter span, decomposition
- [x] Confidence threshold — minScore 0.70, from a 5×9 sweep; `--min-score` override, announced
- [x] Top-1/top-2 margin — minMargin 0.10, against the best DISAGREEING runner-up; exact ties never accepted
- [x] Decision confidence — from the margin, separate from similarity and score
- [x] Vetoes — join keys; non-filterable fields with an operator
- [x] Cover selection — clean before defective, accepted before ambiguous (carried from Phase 3)
- [x] Sentence verdict — AMBIGUOUS_MATCH / NO_SEMANTIC_MATCH as a coded, redacted `VerdictError`
- [x] `--rank` and `--rank-probe` CLI modes
- [x] Ambiguity fixture `ambiguity.json` + two probe sets (48 scored rows)
- [x] Ambiguity tests — 46 ranking tests, 11 CLI tests, 11/11 fix mutations caught
- [x] Redaction — `corpus.DisplayID`, redacting `Candidate` formatting, misspelling leak sweep
- [x] Exit criterion — final 33/34 vs raw 20/34 and 17/18 vs 13/18, zero silently wrong
- [x] Decision 012 — `amount` is contextual, no global rule; regression cases + gate checks
- [x] Decision 013 — `--rank` is diagnostic output; error/log paths withhold user text
- [x] Manual gate passed (2026-09-21, 81/81)

## Phase 5 — Binding / Schema Graph
- [x] Semantic binder — `internal/binding`, 17 positional + schema rules (decision 014)
- [x] Schema graph — `internal/graph`: entity→entity edges both directions, join keys, cardinality per step
- [x] Relationship traversal — simple paths within `maxJoins`, deterministic order
- [x] Join-path validation — mentioned relationships steer; ties → AMBIGUOUS_MATCH; none → RELATIONSHIP_NOT_FOUND
- [x] Multi-condition binding — AND/OR from connectives, mixed refused; conditions on several entities, each on its own path
- [x] Count and exists through relationships, decided by configured cardinality
- [x] Aggregates, top-N (count + metric), time windows, selections
- [x] Negation by operator complement; no opposite → refused
- [x] Value resolution — enum/alias strict, string literals, number/date typing
- [x] Required rejections — all five, plus unexplained words and maxFilters
- [x] `--bind` CLI mode; `multipath.json` fixture (two join paths + a disconnected entity)
- [x] Tests — graph + binding + CLI, 15/15 rule mutations caught, redaction sweep with teeth
- [x] Manual gate passed (2026-09-21) — strict unexplained words kept; "one of" → Phase 8 (decision 016)

## Phase 6 — Deterministic Compiler
- [x] SemanticPlan — `internal/plan`: config names only, no SQL; strict JSON; redacted by default
- [x] Planner — `plan.FromBinding`: accounts for every binding element; dates resolved with the request clock
- [x] SQL grammar — `internal/compiler`: check → lower (grammar nodes) → render (decision 015)
- [x] SELECT — default columns from `selectable` flags, in config order; DISTINCT for listings
- [x] FROM — the root entity's table
- [x] JOIN — to-one paths as LEFT JOIN chains; the aggregated to-many path INNER; shared prefixes joined once
- [x] WHERE — compare / half-open range / EXISTS / correlated COUNT; one connective
- [x] GROUP BY — selections beside aggregates
- [x] HAVING — in the grammar and tested; reachable only from hand-written plans (binding cannot express it)
- [x] ORDER BY — always NULLS LAST; top_n direction from config `order` (new optional key)
- [x] LIMIT — literal int, only with ORDER BY
- [x] Parameter binding — every value a `$n`, numbered in emission order; LIKE wildcards owned by the compiler
- [x] Compiler tests — 5 goldens, 5 refused plans, fan-out rules, allowlists, fuzz (12.6M execs), 27/27 rule mutations caught
- [x] `--compile <plan.json>` (no model) and `--plan <sentence>` (stops at the plan)
- [x] Regression tests: nothing silently discarded — currency/percent/unit refused, accounting per sentence, reflection tripwire on binding types, strict plan decoding
- [x] Decisions 015 (grammar/compiler) and 016 (Phase 5 gate decisions)
- [x] Manual gate passed (2026-09-21) — four gate decisions recorded in 017

## Phase 7 — End-to-End
- [x] Phase 6 closure applied first: four gate decisions (017), gate re-run 125/125, race clean
- [x] Pipeline orchestration — `internal/pipeline`: extract → retrieve → rank → bind → SemanticPlan → compile; one path for --query and every diagnostic mode (decision 018)
- [x] `--query` — final SQL + parameters (values shown: the deliverable), `--explain`, `--json`, `--now`/`--tz`
- [x] Telemetry — `pipeline.Stats`, section 24 fields, no user text or values, tested (decision 013's obligation)
- [x] 50+ manual queries — 74 reviewed cases, expectations written before the first run: 63/74 (queries 48/57, refusals 15/17)
- [x] Golden outputs — `phase7-e2e.golden`, diffed by the gate; byte-identical across processes
- [x] Failure fixtures — every failure classified by the section 18 taxonomy; value readings on similarity alone measured (1/74)
- [x] Valid SQL — 55/55 outputs parse as one PostgreSQL SELECT (libpg_query via pglast, parse-only, outside the project)
- [x] Replaced `TestQueryIsNotImplemented` with `TestQueryIsDerivedOrRefused` + `TestBriefExamplesEndToEnd`
- [x] Config `ranking` thresholds (carried from Phase 4); `maxInputLength` enforced
- [x] Finding M06 fixed — contradictory AND refused (compiler), mutation-tested
- [!] First gate: kept OPEN — V02 was a silently narrower query
- [x] 019 §1 — a value needs literal evidence (ranking veto); regression tests at ranking + pipeline; mutation-verified
- [x] 019 §5 — a shared-word fragment under the threshold is not settled by context (X03); two broader variants measured and rejected
- [x] 019 §2/§3 — top-N metric never inferred; "what is" stays refused; pinned by tests
- [x] Re-measured: Phase 4 probes 33/34 · 17/18 (0 silently wrong), calibration sweep regenerated, query set 68/74 with 0 silently wrong accepted, 53/53 valid PostgreSQL, no regressions
- [x] Re-gate: 019 §5 revised by the user — a shared phrase needs distinguishing evidence in the sentence's words; "show customer date" (X02) refused; similarity no longer decides; refusal message now gives its real reason (was a false "margin 0.294 < 0.100")
- [x] Regression tests for the exact X02 case incl. reason/evidence; the user's accept examples; 9/9 rule mutations caught
- [x] Query set: X02 expectation changed by decision; X10–X12 added → 70/77, 0 silently wrong accepted, 54/54 valid PostgreSQL; X11 "show customer order date" a *known* safe refusal (cover tie + to-many binding, out of scope)
- [x] Re-measured: Phase 4 probes byte-identical (33/34 · 17/18), calibration sweep regenerated (0 silent wrongs in every cell)
- [x] Manual gate passed (2026-09-22) — phases 0–7: 146/146, 0 skipped; `go test -race -count=1 ./...` clean

## Phase 8 — Security
- [x] Read-only enforcement — SQL write keywords refused explicitly (SECURITY_REJECTED) unless a configured phrase explains them literally; before ranking's verdict; in every sentence mode (021 §2)
- [x] Identifier allowlist — the output guard re-checks every identifier in the finished SQL against the config (021 §3), on top of the registry lookup
- [x] Parameterization — the guard admits no string literal but ESCAPE's `'\'`; placeholders in order, one per parameter
- [x] Operator/function allowlist — the guard's closed keyword lexicon, on top of the compiler's switches
- [x] Join allowlist — graph.Walk (unchanged) plus the query-wide join count
- [x] Complexity limits — all eight section 16.7 limits enforced, each defined (021 §1): `maxPhrases` 64→256 (spans; Phase 3's own limit), `maxCandidates` per span (user's choice), `maxJoins` query-wide union, new `maxNestingDepth`, `maxPlanningMillis` over the deterministic stages
- [x] Ambiguity rejection — 16.8 finding "update date" (user-scoped): a generic word qualified by an unconfigured word is rejected (021 §6); 4 silent wrongs refused, probes and the Phase 7 query set byte-identical, alternatives measured
- [x] Adversarial tests — 57-case set, 53/57 (the 4 failures are recorded findings), every case a safe query or an explicit refusal; 69/69 accepted queries valid PostgreSQL; `FuzzPipeline`
- [x] Config warning channel — 019 §4 (phrases shared across kinds) and 016 (phrases that never form), `internal/configlint`
- [x] 16.11 finding fixed — the embedding worker contacted huggingface.co on every start; now offline by default, zero connections, revision from the local cache
- [x] 16.10 audit — no new leak; every new message is structural
- [x] Mutation-tested — 38/38 caught (first run 29/38: 5 non-compiling mutations rewritten, 4 test gaps closed)
- [!] First gate (2026-09-22): kept OPEN until four items were addressed (decision 022)
- [x] 022 §1 semantic completeness — a qualifier added to a configured phrase is refused unless the whole phrase is configured or the word has another role (owner entity, relationship endpoint, a value's field); phrases = touching spans that agree; request verbs in noun position ("list price") count as content and need a configured phrase literally. P10, P11 + 4 exploration silent wrongs refused; controls unchanged
- [x] 022 §2 numeric precision — `internal/numeric.Decimal` (exact, zero value is not a number, PostgreSQL numeric range) from extraction through binding, SemanticPlan and parameters; `Driver()` never float64. V02 exact; also fixed: sign and leading point dropped, European grouping and unknown tags misread, malformed tokens dropped, spelled scales summed, window counts truncated, LIMIT past 2^53, contradiction by float equality, periods past year 9999
- [x] 022 §3 input boundary — `;`, `--`, `/*`, `*/` (and their NFKC forms, on the normalised text) are SECURITY_REJECTED before extraction; verified no query and no later stage (pipeline, api, live POST /v1/noLLM/tosql, fuzz invariant)
- [x] 022 §4 maxPhrases 256 approved and documented; resource-bound tests retained (input length both directions added), run by gate section 22
- [x] Re-measured: Phase 8 set 89/91 (57 + 34 new; 2 known = new findings G33/G34), Phase 7 set 71/77 with 0 silently wrong (only Z04/Z05 changed), probes byte-identical, sweep identical in 45 cells, 80/80 valid PostgreSQL, race clean in 19 packages, fuzz 767,112 inputs with 2 new invariants, mutation 31/31; scripts/run-manual-tests.sh 167/167 (0 skipped)
- [x] Re-gate (2026-09-23): the user approved §5 and scoped the `list price of products` investigation
- [x] 022 §5 explicit-vs-similarity — if the sentence explicitly names a configured target (a content word of its configured names, synonyms, phrases or values), another independent span may not resolve to that same target by similarity alone. A field and its own VALUE are not the same target (Phase 7 V03 measured); paraphrase with its own configured evidence is untouched. G33, G34 refused; no other outcome changed anywhere
- [x] 022 §6 the config decides the opening request verb — a configured surface of 2+ words running from the verb takes it out of the request frame, and the reading must then name that target literally. Investigation: `list price of products` is a SEPARATE issue (the verb has the request role, nothing is discarded); measured 18 phrasings over two configs, the configured field always won; commerce has no such concept, so the command reading stands. No measured cost
- [x] Re-measured (round 3): Phase 8 set 93/93 (0 known failures), Phase 7 byte-identical 71/77, probes and sweep unchanged, 80/80 valid PostgreSQL, race clean 19 packages, fuzz 1,418,328 inputs, mutation 41/41 (10 new on §5/§6, 3 survivors closed), zero silently wrong accepted queries in every corpus; scripts/run-manual-tests.sh 170/170 (0 skipped)
- [x] Manual gate passed (2026-09-23) — final gate: 170/170 manual checks (0 skipped),
      all Phase 0–7 regressions pass, Phase 7 query set byte-identical, 80/80 valid
      PostgreSQL, 41/41 mutations caught, race clean, 1,418,328 fuzz inputs clean,
      **zero silently wrong accepted queries** in every corpus
- [x] G36 `list price of products` decided at the final gate: kept compiling, no further
      refusal rule, recorded as intentional behaviour — a known semantic interpretation
      (022 §7), not a bug and not a deferred finding

## Phase 9 — Performance
- [x] Benchmark harness — `cmd/qf-bench` (separate binary, decision 023 §1) over
      `internal/bench`; every scenario merges into one raw JSON document and the report is
      rendered from it, never typed. Telemetry comes from `pipeline.Stats`, never stdout
      (decision 013's carried-forward obligation, now discharged)
- [x] Cold-start benchmark — model load 1.63s floor; 100 entries ready in 1.77s, 10k in 12.63s
      (index build 10.25s); with a cached index 10k is 1.88s
- [x] Warm benchmark — 25 reviewed sentences × 20 passes per scale, warmup discarded
- [x] p50 — 6.2ms / 14.9ms / 103.7ms at 100 / 1k / 10k entries
- [x] p95 — 12.9ms / 35.8ms / 260.2ms
- [x] p99 — 14.4ms / 40.3ms / 293.2ms (nearest-rank; under-sampled tails marked)
- [x] CPU — per process, Go and worker separately (getrusage + ps)
- [x] RAM — Go heap 2.6 / 14.6 / 74.6MB; the model process is ~0.4GB at every scale
- [x] 100-entry schema — generated, exact (290 index vectors, 6 entities, 43 fields)
- [x] 1k-entry schema — exact (2,576 vectors, 75 entities, 529 fields)
- [x] 10k-entry schema — exact (25,418 vectors, 768 entities, 5,371 fields)
- [x] concurrency 1/10/50/100 — all three scales; throughput saturates at c=10 and never improves
- [x] Layer comparison (section 17's three depths) — retrieval / +ranking / full: the
      deterministic stages add under 1ms at every scale
- [x] Embedding vs vector-search split — `Stats` reports only their sum; the harness times both.
      Embedding flat at ~5.5-6ms; the scan grows 1.2 → 11.1 → 109ms (decision 023 §5)
- [x] Answer invariance across scale — 0 of 25 sentences changed at 1k or 10k
- [x] v1 (LLM) baseline — out of process against a prebuilt production binary (decision 023 §7);
      run on a degraded provider window and labelled as such
- [x] Performance report — `benchmarks/reports/performance.md`, regenerated with `--suite report`
- [x] `--suite verify` — recomputes every published percentile, count and throughput from the
      per-request samples stored beside it; 24 scenarios, 0 mismatches
- [x] Gate section 23 added to `scripts/run-manual-tests.sh` (19 checks)
- [ ] Manual gate passed
- **Findings for the gate** (`docs/phase-9-results.md`): P01 the index is a full scan, so latency
      is linear in schema size (94% of a 10k request); P02 one embedding worker serialises
      everything, capping throughput at ~185 req/s; P03 the worker's 2,048-text batch limit means
      the shipped CLI cannot index a 1k-entry schema at all (chunked only in the harness); P04 ten
      index scans per sentence multiply the scan cost; P05 the deterministic stages are free;
      P06 retrieval quality holds at 100× the schema size

## Phase 10 — Evaluation
- [ ] 300–500 custom queries
- [ ] Public benchmark subset
- [ ] Entity accuracy
- [ ] Field accuracy
- [ ] Operator accuracy
- [ ] Value accuracy
- [ ] Join accuracy
- [ ] Aggregation accuracy
- [ ] Full-query accuracy
- [ ] Ablation A
- [ ] Ablation B
- [ ] Ablation C
- [ ] Ablation D
- [ ] Ablation E
- [ ] Failure taxonomy
- [ ] Final research report
- [ ] Go/No-Go decision

## Phase 11 — Optional Backends
- [ ] Only start after Phase 10 approval

---

## Carried forward

Work deliberately left for a later phase, recorded here so it is deferred rather
than forgotten.

| Item | Deferred to | Why |
|---|---|---|
| ~~Full config validation~~ | ~~Phase 1~~ | **Done** — 23 named rules, 18 fixtures |
| ~~`relationships` decoded into a typed shape~~ | ~~Phase 1–5~~ | **Done** — typed `Relationship` with endpoints and cardinality |
| Strict decoding of unknown config keys | not planned | Decided against in 005: fixtures are shared across phases, and a strict decoder would make a Phase 5 fixture unreadable by a Phase 2 binary |
| ~~Confidence threshold and top1/top2 margin config~~ | ~~Phase 4~~ | **Done** — `ranking.Policy`, CLI flags; kept out of SecurityPolicy as planned |
| ~~Ranking policy as a config-file key~~ | ~~Phase 7~~ | **Done** — optional `ranking` block; default → config → flags (decision 018) |
| Ranking recalibration on a real evaluation set | Phase 10 | 0.70 / 0.10 comes from hand-written rows by the ranker's author. The sweep regenerated after decision 019 shows 0.60 / 0.10 at 34/34 + 18/18 with 0 silently wrong (chosen: 33/34 + 17/18, 0) — noted, not acted on |
| Window-based attachment of operators and values | Phase 5 | A heuristic (4 tokens right, stops at connectives/subjects); the binder replaces it with structure |
| Unexplained content words | Phase 5 | Reported by ranking, not judged; whether one must fail the query is a binding decision |
| ~~Commerce `amount`: accept order.amount or treat as ambiguous?~~ | ~~user's call~~ | **Decided** — keep; contextual, no global rule (decision 012) |
| ~~Telemetry built from redacted structured forms, never `--rank` stdout~~ | ~~Phase 9~~ | **Done** — `cmd/qf-bench` decodes `pipeline.Stats` and never parses CLI output; the raw results document holds no sentence text, no values and no SQL (decision 023 §4) |
| ~~V02: values accepted on similarity alone~~ | ~~user's call~~ | **Done** (019 §1): a value needs literal evidence |
| Brief example 3 as worded | kept refused | **Decided** (019 §2): the metric must be named; "spent … last year" needs an explicit temporal-aggregation pattern (not planned) |
| "what is …" questions | future feature | **Decided** (019 §3): refused; a deliberate grammar/corpus feature later |
| ~~Phrases shared by incompatible semantic kinds ("product", "placed", "highest")~~ | ~~Phase 8~~ | **Done** (021 §4): `phrase_shared_across_kinds` warnings via `--validate-config` |
| ~~"show customer date" (X02): X03's shape, but over the threshold, so context decides~~ | ~~user's call~~ | **Decided** (019 §5, revised): refused — a shared phrase needs distinguishing evidence in the sentence's words |
| "show customer order date" (X11): refused by a cover tie ("customer order" vs "order date", both exact, broken by position) and then by the to-many listing rule | separate scoped change | **Decided** (020): stays a safe, known refusal, and Phase 7 stays closed. Not Phase 8 work unless Phase 8's scope names it. Fix scope: overlapping-phrase cover selection, relationship-aware resolution, binding a related field under a valid root, no accidental position tie-break. Domain-agnostic; no phrase-specific rule |
| Values in inflected forms ("living") | config | Add aliases, or a deliberate configured normalisation — never stemming inside ranking (019) |
| ~~`security.maxCandidates` enforcement~~ | ~~Phase 8~~ | **Done** (021 §1): per span, by the user's choice; startup and per-request checks |
| ~~Explicit read-only rejection ("delete all customers")~~ | ~~Phase 8~~ | **Done** (021 §2): SECURITY_REJECTED unless a configured phrase explains the keyword |
| ~~top_n direction (highest vs lowest)~~ | ~~Phase 6~~ | **Done** — optional `queryPatterns[].order`; missing → CONFIG_ERROR, never guessed (decision 015) |
| ~~Config phrases containing number words ("one of")~~ | ~~Phase 8~~ | **Done** (021 §4): `phrase_never_forms`, checked with Phase 3's own parser (dates too) |
| Unexplained words refused ("customers who bought something") | kept | **Decided** (016): strict, and no "harmless words" list. Revisit only on measured Phase 10 cost |
| Per-field unit/currency metadata (`"currency": "USD"`, `"unit": "kg"`) | later (unscheduled) | **Decided** (017 §1): until it exists, currency/percent/unit values are VALUE_RESOLUTION_FAILED; no common-knowledge interpretation, no conversion |
| AND conditions through one to-many relationship ("same row or any rows?") | kept | **Decided** (017 §2): stays AMBIGUOUS_MATCH; no global same-record rule |
| Include-unknown / NULL policy for negation | only if asked | **Decided** (017 §3): standard SQL NULL semantics are intentional; `TestNegationKeepsStandardNullSemantics` guards it |
| Primary keys in the config | if needed | Would allow ORDER BY tie-breakers and DISTINCT by key; neither is required for correctness now |
| ~~Query-wide join/complexity limits~~ | ~~Phase 8~~ | **Done** (021 §1): `maxJoins` also query-wide (union of paths), `maxNestingDepth` added |
| ~~Unconfigured word inside a wholly present configured phrase ("show product discount price" → product.price)~~ | ~~user's call~~ | **Done** (022 §1): scoped at the gate; a qualifier is never silently discarded |
| ~~Numbers beyond float64's exact range ("more than 99999999999999999999999" → 1e23)~~ | ~~user's call~~ | **Done** (022 §2): the user chose exactness over refusal; numbers are exact decimals end to end |
| ~~Two requests in one input ("customers from India; select * from orders")~~ | ~~user's call~~ | **Done** (022 §3): statement and comment syntax refused at the input boundary |
| ~~A similarity-only reading of a target the sentence names elsewhere ("products with price and discount above 10", "price of discount" → price)~~ | ~~user's call~~ | **Done** (022 §5): approved at the re-gate and built; both refused, nothing else changed |
| "show product cost price", "show customers list", "show price list" refused | kept | The costs of 022 §1: mixing two surfaces of one target, and a request verb with no function word after it |
| Worker parallelism: one stdio worker serialises every embed call (`internal/embedding.Worker.mu`) | **measured; the pool is a gate decision** | Phase 9 finding P02: throughput saturates at ~185 req/s (100 entries) from c=10 onward and latency then grows linearly with concurrency. The user scoped Phase 9 to measurement only, so no pool was built |
| The vector index is a full scan (`retrieval.Index.Search`) | **gate decision** | Phase 9 finding P01: 1.2 / 11.1 / 109ms of scan at 290 / 2,576 / 25,418 vectors, 10 scans per sentence. Diagnosed, not optimised (brief rule 13) |
| The worker refuses a batch over 2,048 texts, and `retrieval.Build` embeds in one call | **gate decision** | Phase 9 finding P03: no schema past ~800 entries can be indexed by the shipped CLI. Chunking exists only in `internal/bench.Batched`; where the real fix belongs is for the gate (decision 023 §6) |
| ~~"list price of products" (G36) → price~~ | ~~user's call~~ | **Decided** (022 §7, final gate): intentional behaviour, a known semantic interpretation — kept compiling, no further refusal rule, not a bug. Investigated at the re-gate (022 §6): a separate issue from semantic completeness — the verb has the request role. A config that has the term wins in every measured phrasing (18, two configs), and §6 guarantees it; commerce has no such concept, so the command reading stands. The remedy for a config that means it is to configure it |
| Exponent notation ("1e5") and ordinals ("30th") refused | kept | The cost of 022 §2: only the notation it reads exactly (a config-driven locale is also unplanned) |
| Any ";" or "--" in a request refused, even inside a value | kept | The cost of 022 §3: one request is one query |
| "signup date" refused | kept | The cost of 021 §6: "signup" is configured nowhere; a config that means created_at adds the synonym |
| ~~`qferr.CodeOf(nil)` is `INTERNAL_ERROR`~~ | ~~user's call~~ | **Done** (017 §4): every CodeInternal assertion also asserts `err != nil`, incl. Phase 4's test; standing rule for later phases |
| LIKE case sensitivity | Phase 10 | `LIKE`, not the PostgreSQL-only `ILIKE`; revisit on measured misses |
| Counting through relationships that do not touch the root | if needed | Refused: what to count is undetermined |
| ~~`PatternIntent` enum revisited against a real planner~~ | ~~Phase 6~~ | **Done** — kept as is: top_n gains `order`; count → aggregate; time_window/filter add no plan content; aggregate intent refused (nothing to plan) |
| Operator name translation table for v1↔v2 comparison | Phase 10 | v2 uses the brief's short ids, v1 uses `equals`/`notEquals`/`contains` (decision 004) |
| ~~`--explain` producing real output~~ | ~~Phase 5–7~~ | **Done** — `--query --explain`: reading, binding, plan, up to the refusing stage |
| ANN index (FAISS or similar) | Phase 9, if needed | A full scan of 173 vectors is under 100 µs; adding approximation before profiling would add error to the thing being measured |
| Worker pool for concurrent embedding | Phase 9 | One worker serialises requests; whether that is a bottleneck is a measurement, not a guess |
| ~~Multi-span queries (`client location`, `recent customer`)~~ | ~~Phase 3~~ | **Done** — both now split into separate spans; both are gate regression checks |
| ~~Cover selection: choosing a non-overlapping reading from overlapping spans~~ | ~~Phase 4~~ | **Done** — `chooseCover`, span quality as primary key |
| Lemmatisation, POS tagging, full NFKC | if measured | Each is a bounded addition once a Phase 10 failure points at it; none is justified by evidence now (decision 009) |
| Languages other than English | not planned | The number, unit, month and function-word lists are English |
| ~~Relationships losing to entities on raw similarity~~ | ~~Phase 4~~ | **Done** — span quality + relationship evidence; regression-tested |
| ~~Exact-token matches scoring 1.000 on ambiguous words~~ | ~~Phase 4~~ | **Done** — margin + tie rule; ambiguous under every policy |
| ~~Value-entry IDs printed unredacted by `--search`/`--probe`~~ | ~~found in Phase 4~~ | **Done** — `corpus.DisplayID`; a Phase 2 output change, made as a contract fix |
| A stricter mode that redacts identifiers too | not planned | Would need `Violation.Path` redacted as well; should be a configured mode, not a quiet change (decision 006) |
| ~~`TestQueryIsNotImplemented` in `cmd/qf-v2/main_test.go`~~ | ~~Phase 7~~ | **Replaced** — `TestQueryIsDerivedOrRefused` (CLI) + `TestBriefExamplesEndToEnd` (pipeline) |
