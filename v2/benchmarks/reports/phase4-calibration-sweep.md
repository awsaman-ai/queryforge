# Phase 4 calibration sweep

**Re-run 2026-09-22 (Phase 8 gate, round 2)** after decision 022 changed
ranking's count test (`isCount` is now exact, on a decimal) and extraction
(numbers read exactly; malformed ones reported): the table below is again
**identical in every one of the 45 cells**, and both probe reports at the
chosen policy are byte-identical. Decision 022's other rules run in binding and
at the input boundary, after or before ranking, so they cannot move a cell.

**Re-run 2026-09-22 (Phase 8)** after decision 021 §6 changed ranking (a
generic word qualified by an unconfigured word is rejected as a reading): the
table below is **identical in every one of the 45 cells**, and both probe
reports at the chosen policy are byte-identical. No cell gained or lost a
correct answer or a silent wrong.

**Regenerated 2026-09-22 (Phase 7 re-gate)** after decision 019 §5 was
revised: a phrase shared by several targets' configured surfaces is refused
unless the sentence's own words spell out one target's phrase, whatever its
similarity. Real model, MiniLM-L6-v2 @ 1110a243. Each cell is
`correct/total/silently-wrong` for the ambiguity set (34 scored rows) `+` the
commerce set (18 scored rows), from `--rank-probe` with `--min-score` /
`--min-margin`.

The chosen policy (decision 011) is **minScore 0.70, minMargin 0.10: 33/34/0 +
17/18/0**, unchanged, and both probe reports at that policy are byte-identical
to the previous ones. **Every cell now has zero silent wrongs**, including the
minMargin 0.00–0.08 columns, which had up to 2 + 2 before.

What moved, row by row (diffed against a build of the first §5 rule):

- **minMargin below 0.10.** The lone words `date` and `value` were accepted on
  margins of 0.005 to 0.094 when the policy allowed it: silent wrongs, in both
  sets. They are now refused whatever the margin: each word is part of
  several fields' configured phrases, and nothing else in a one-word sentence
  distinguishes them.
- **minScore 0.75 and 0.80, ambiguity set, 31 → 32.** `orders by date` (date at
  0.726) was refused by the first rule because it sat under the threshold. It
  is now accepted as order.created_at, because the sentence spells out
  "order date".

Observation, not acted on: **0.60 / 0.00–0.10 scores 34/34/0 + 18/18/0**, better
than the chosen policy on these 52 rows. The rows are hand-written by the
ranker's author, and recalibration on a real evaluation set is scheduled for
Phase 10 (TODO), so the policy is not retuned on this evidence.

| minScore | 0.00 | 0.03 | 0.05 | 0.08 | 0.10 | 0.12 | 0.15 | 0.20 | 0.30 |
|---|---|---|---|---|---|---|---|---|---|
| 0.60 | 34/34/0 + 18/18/0 | 34/34/0 + 18/18/0 | 34/34/0 + 18/18/0 | 34/34/0 + 18/18/0 | 34/34/0 + 18/18/0 | 34/34/0 + 17/18/0 | 34/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 25/34/0 + 13/18/0 |
| 0.65 | 33/34/0 + 18/18/0 | 33/34/0 + 18/18/0 | 33/34/0 + 18/18/0 | 33/34/0 + 18/18/0 | 33/34/0 + 18/18/0 | 33/34/0 + 17/18/0 | 32/34/0 + 17/18/0 | 32/34/0 + 17/18/0 | 24/34/0 + 13/18/0 |
| 0.70 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 24/34/0 + 12/18/0 |
| 0.75 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 23/34/0 + 11/18/0 |
| 0.80 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 23/34/0 + 11/18/0 |

## First decision-019 sweep (§1 and the first §5, earlier on 2026-09-22)

Kept for comparison. Format: `correct/total/silently-wrong`.

| minScore | 0.00 | 0.03 | 0.05 | 0.08 | 0.10 | 0.12 | 0.15 | 0.20 | 0.30 |
|---|---|---|---|---|---|---|---|---|---|
| 0.60 | 32/34/2 + 16/18/2 | 34/34/0 + 17/18/1 | 34/34/0 + 17/18/1 | 34/34/0 + 17/18/1 | 34/34/0 + 18/18/0 | 34/34/0 + 17/18/0 | 34/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 25/34/0 + 13/18/0 |
| 0.65 | 32/34/1 + 16/18/2 | 33/34/0 + 17/18/1 | 33/34/0 + 17/18/1 | 33/34/0 + 17/18/1 | 33/34/0 + 18/18/0 | 33/34/0 + 17/18/0 | 32/34/0 + 17/18/0 | 32/34/0 + 17/18/0 | 24/34/0 + 13/18/0 |
| 0.70 | 32/34/1 + 16/18/1 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 17/18/0 | 33/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 32/34/0 + 16/18/0 | 24/34/0 + 12/18/0 |
| 0.75 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 23/34/0 + 11/18/0 |
| 0.80 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 16/18/0 | 31/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 31/34/0 + 15/18/0 | 23/34/0 + 11/18/0 |

## Previous sweep (before decision 019; 32 + 16 scored rows at the time)

Kept for comparison. Format: `correct/silently-wrong`.

| minScore | 0.00 | 0.03 | 0.05 | 0.08 | 0.10 | 0.12 | 0.15 | 0.20 | 0.30 |
|---|---|---|---|---|---|---|---|---|---|
| 0.60 | 30/2 + 14/2 | 32/0 + 15/1 | 32/0 + 15/1 | 32/0 + 15/1 | 32/0 + 16/0 | 32/0 + 15/0 | 32/0 + 15/0 | 31/0 + 15/0 | 23/0 + 11/0 |
| 0.65 | 30/1 + 14/2 | 31/0 + 15/1 | 31/0 + 15/1 | 31/0 + 15/1 | 31/0 + 16/0 | 31/0 + 15/0 | 30/0 + 15/0 | 30/0 + 15/0 | 22/0 + 11/0 |
| 0.70 | 30/1 + 14/1 | 31/0 + 15/0 | 31/0 + 15/0 | 31/0 + 15/0 | 31/0 + 15/0 | 31/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 22/0 + 10/0 |
| 0.75 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 13/0 | 29/0 + 13/0 | 29/0 + 13/0 | 21/0 + 9/0 |
| 0.80 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 14/0 | 30/0 + 13/0 | 29/0 + 13/0 | 29/0 + 13/0 | 21/0 + 9/0 |
