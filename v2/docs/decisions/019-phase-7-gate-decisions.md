# 019 — Phase 7 gate decisions: explicit evidence for values, no inferred metrics

**Status:** accepted (user decisions at the Phase 7 gate; §5 revised by the
user at the re-gate, after which Phase 7's gates were re-run green and the
phase closed) · **Phase:** 7 →
4, 5, 8 · **Date:** 2026-09-22

The user kept Phase 7 open because finding V02 was a semantic correctness
failure: the engine silently answered a narrower question than the one asked.
Six decisions follow. §1 and §5 change **Phase 4 ranking behaviour**. No
interface changed, and every Phase 4 number was re-measured (see Evidence).

## §1 — A value needs explicit evidence (finding V02)

A value candidate (an enum value or a value alias) is **accepted only when the
sentence contains it literally**: its configured surface is present in the
span. Lexical folding is the same light folding every lexical test already
uses (case, underscores, one plural *s*). There is no stemming.

- Retrieval still **discovers** value candidates. It cannot **authorise**
  one: a value candidate without lexical evidence is vetoed. It then can be
  neither the reading nor a competitor, because a value the sentence does not
  contain cannot be what it meant.
- "customers living in India": "living" is not a configured value or alias of
  anything. The alias "live" is vetoed, "living" is left unexplained, and the
  request is refused (UNSUPPORTED_QUERY). It never becomes `status = 'active'`.
- To make "living" work, a config author adds it as an alias. Nothing in Go
  changes.
- Regression tests: `TestValueNeedsLiteralEvidence` and
  `TestLiteralValueEvidence` (ranking), and `TestNoValueIsBoundOnSimilarityAlone`
  (pipeline). They are mutation-verified: with the veto removed, all three
  fail.
- Two Phase 4 tests encoded the old contract, in which an absent value
  ("Sylvania" for "freedonia") competed on similarity. They were rewritten to
  the new one. Value-versus-value ambiguity is still tested, with a word
  configured on two fields, so that both readings *are* literal.

## §2 — A top-N query must name its metric

The rule is kept. The metric is **never** inferred from a number field
compared elsewhere in the sentence. "spent … last year" is not "top-N by
spending last year": that would need an explicit temporal-aggregation pattern,
which does not exist. `top 10 customers by spending from India` is valid
because the metric is named. Pinned by `TestTopNMetricIsNeverInferredFromAComparison`
and `TestBriefExample3IsRefusedAtBinding`.

## §3 — "what is …" stays refused

A generic "what is" pattern is **not** added in Phase 7. It would change the
query grammar and the corpus, and it is to be a deliberate future feature.

## §4 — Phase 8: warn on phrases shared by incompatible semantic kinds

Phase 8's config validation adds a **warning** (not an error, unless the
collision is provably unusable) for a phrase configured on targets of
incompatible semantic kinds: "product" on an entity and a relationship,
"placed" on a relationship and a field, "highest" on an aggregation and a
pattern. The warning names the phrase, both semantic kinds and both config
locations. The redaction contract applies: a phrase that is a config
**value** (an alias or enum value) is redacted by default, like every other
value. This sits beside decision 016's number-word check. Both need the
validator's new warning channel.

## §5 — A shared phrase needs distinguishing evidence (findings X03, X02)

**Revised at the re-gate (2026-09-22).** The first version below refused only
readings under the threshold, and so left "show customer date" (X02) to
context. The user closed that gap with a principle, not a word list:

> If a generic semantic phrase is shared by multiple candidates and the
> sentence does not provide explicit distinguishing evidence, refuse.

| Sentence | Outcome | Why |
|---|---|---|
| `show customer date` | **AMBIGUOUS_MATCH** | "date" is in created_at's "joined date" and placed_at's "order date"; "customer" does not distinguish them |
| `show customer creation date` | created_at | "customer creation date" is created_at's configured synonym |
| `show customer order date` | placed_at, for the phrase | "order date" is placed_at's configured synonym. The sentence as a whole is still refused, for older reasons (see *Known limit*) |
| `customer date` configured on created_at | created_at | an explicit config phrase |

**Why "customer" is not evidence.** It names the entity that owns
created_at, but placed_at is one join from customer, and a customer's order
date is just as much a "customer date". Before the revision, the refusal
turned on the score difference, 0.726 (placed_at) against 0.711 (created_at),
plus entity evidence (+0.15 for one, −0.075 for the other). The user's
direction was that this small difference must not decide the answer. The
rule now ignores similarity entirely: whichever field leads, and by however
much, the sentence is refused (`TestSharedPhraseRefusalIgnoresTheSimilarityOrder`).

**The rule, as built** (`ranking.fragmentAmbiguity`). An accepted reading is
refused as AMBIGUOUS_MATCH, naming every sharing target, when all of these
hold:

- it has no lexical evidence (the span is not itself a configured surface);
- the span's words are a strict fragment of its configured name, synonym or
  phrase;
- the same words are a fragment of another non-agreeing target's surface
  anywhere in the corpus;
- the sentence gives no **explicit distinguishing evidence**. Such evidence
  exists only when **one** of the sharing targets has a configured surface
  holding the fragment whose every word appears in the sentence, and that
  target is the leading reading.

What counts as evidence, and what does not:

- **Counts:** a configured phrase spelled out by the sentence's own words,
  contiguous or not. In "orders by date", "order date" is spelled out and
  belongs to placed_at only. This case is why the evidence is not required to
  be contiguous: the uniform variant (below) refused it.
- **Does not count:** entity context, similarity or its order, a *partial*
  surface ("customer" is in "customer creation date", but "creation" is
  missing, and the qualifier is what makes the synonym specific), and
  descriptions (prose shares common words everywhere).
- **Refused, not switched:** when the spelled-out phrase belongs to a target
  other than the leading one, or phrases of several targets are spelled out.
  The rule never chooses a reading; it only withholds one.

No "shared words are ambiguous" list exists. Whether a word is shared is read
from the config, and whether it is settled is read from the sentence.

**The refusal states its real reason.** The first version reused the
margin message and printed, for X03, "insufficient confidence separation …
(margin 0.294 < 0.100)". That is false, since 0.294 clears the bar. A
shared-phrase refusal is now marked (`SpanRanking.SharedPhrase`) and renders
as "the phrase at tokens[2:3] is part of the configured phrases of several
targets, and nothing in the sentence distinguishes them: field:customer.created_at,
field:order.placed_at". It stays redaction-safe: token positions and
structural targets only, and the phrase is quoted only under `--debug`.

**Scope held.** No retrieval, scoring, weight, threshold, cover-selection or
binding change was made. The rule is the same post-decision check, with its
similarity condition removed and the evidence exemption added.

**Tests.** Ranking:
`TestSharedPhraseWithoutDistinguishingEvidenceIsRefused` (the exact case,
MiniLM's scores; it asserts the code, both targets, the recorded evidence and
reasons, a margin that *would* have accepted, and the message),
`TestSharedPhraseRefusalIgnoresTheSimilarityOrder`,
`TestSharedPhraseWithDistinguishingEvidenceResolves` (the user's three accept
examples and "orders by date"), `TestSharedPhraseEvidenceMustSingleOutTheReading`
and `TestSharedPhraseIgnoresDescriptions`. Pipeline:
`TestSharedDateIsNotSettledByCustomer`. Nine mutations of the rule were run;
**all nine are caught**. One (counting descriptions) survived the first run,
and `TestSharedPhraseIgnoresDescriptions` was added for it.

**Known limit: "show customer order date".** The rule accepts placed_at for
"order date" there, but the sentence is still refused, safely, for two older
reasons that are not this rule:

1. "customer order" (the relationship `customer_orders`, an exact surface) and
   "order date" (placed_at, an exact surface) overlap and tie on confidence and
   length. Cover selection breaks the tie by position and takes "customer
   order", which leaves "date" ambiguous by margin (0.015).
2. With the other cover, binding refuses listing order fields through
   customer ("root the query at order", decision 015).

Resolving it needs a cover-selection and a binding change, which the user
ruled out of scope. It is query-set case X11, marked *known*.

**Alternatives measured:**

| Variant | Phase 4 probes | Query set | Why not |
|---|---|---|---|
| *Similarity floor*: no lexically unsupported reading below the threshold, shared or not | 33/34 · 17/18 | 60/74 | Breaks three correct date queries (D01–D03: "created", "registered", "joined" are single-home words carried by context) |
| *Uniform*: every shared fragment refused, with no evidence exemption | 32/34 · 17/18 | 63/74 | Refuses "orders by date", which spells out "order date" |
| *First version*: shared fragment AND sub-threshold | 33/34 · 17/18 | 68/74 | Let X02 through on a 0.015 difference |
| **Built: shared fragment AND no distinguishing evidence** | **33/34 · 17/18** (reports byte-identical) | **70/77** | — |

## §6 — Phase 6 changes carried in Phase 7: approved

- The contradiction refusal (`country = IN AND country = US` → AMBIGUOUS_MATCH)
  **stays**.
- `--query` **may display parameters by default**: callers need them to
  execute the query. Values stay out of errors and telemetry unless requested
  through the existing debug and diagnostic mechanism.

## Evidence

| Measurement | Before §1/§5 | After §1–§3 + first §5 | After the §5 revision (re-gate) |
|---|---|---|---|
| Phase 4 ambiguity probe | 33/34, 0 silently wrong | 33/34, 0 silently wrong | **33/34, 0 silently wrong** (report byte-identical) |
| Phase 4 commerce probe | 17/18, 0 silently wrong | 17/18, 0 silently wrong | **17/18, 0 silently wrong** (report byte-identical) |
| Calibration sweep at 0.70 / 0.10 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | **33/34/0 + 17/18/0**; regenerated, see `benchmarks/reports/phase4-calibration-sweep.md` |
| Phase 7 query set | 63/74, **1 silently wrong** (V02) + 1 arguable (X03) | 68/74, 0 silently wrong | **70/77, 0 silently wrong accepted queries** |
| Query set: values accepted on similarity alone | 1 | 0 | **0** |
| Previously passing cases that no longer pass | — | none | **none** (X02 changed expectation by decision) |
| Accepted queries valid under PostgreSQL's parser | 55/55 | 53/53 | **54/54** (49 query set + 5 compiler goldens) |

At the first §5, four query-set expectations changed, **only** where §1–§3
specified the outcome: V02, O05, A03 and A04. At the revision, one more
changed, X02, by the user's decision. Three cases were added, with their
expectations written before the run: X10 (`show customer creation date`,
passes), X11 (`show customer order date`, *known* failure, a safe refusal)
and X12 (`orders by date`, passes). Of the 77 cases, the only outcome that
changed is X02's. X03's refusal message now states its real reason.

## What would cause us to revisit it

- A config whose values are routinely written in inflected forms: the answer
  is aliases, or a deliberate, configured normalisation, never stemming inside
  ranking.
- A Phase 10 failure pattern concentrated in §5's refusals.
- A Phase 10 failure pattern where the spelled-out-phrase evidence accepts a
  wrong reading: it is literal, but not contiguous.
- The cover tie in X11 ("customer order" against "order date"), if a later
  phase changes cover selection or binding's to-many listing rule.
