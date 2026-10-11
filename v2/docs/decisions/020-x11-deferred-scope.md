# 020 — X11 stays a known limitation; the scope of its eventual fix

**Status:** accepted (user decision after the Phase 7 close) · **Phase:** none
(a separate, explicitly scoped change) · **Date:** 2026-09-22

Query-set case X11, `show customer order date`, is refused. The refusal is safe.
The reasons are in decision 019 ("Known limit") and `docs/phase-7-results.md`:
a cover tie between "customer order" and "order date" is broken by position,
and with the other cover the to-many listing rule refuses. This record fixes
what happens now and what the eventual fix is allowed to be.

## Now

- X11 **stays refused**, and its golden entry (`FAIL (known)`, AMBIGUOUS_MATCH)
  stays as it is.
- **Phase 7 stays closed.** Its ranking, cover-selection and binding code is
  not reopened to make X11 pass, and its recorded results (gate 146/146, query
  set 70/77, 0 silently wrong, 54/54 valid PostgreSQL) do not change.
- **Phase 8 does not pick it up** as a side task. It becomes Phase 8 work only
  if Phase 8's own scope names it explicitly.

## The eventual fix: a separate change, scoped to

1. selecting a cover among **overlapping semantic phrases**;
2. **relationship-aware** cover resolution;
3. binding a **related entity's field** while keeping a valid query root;
4. stopping a **position-based tie-break** from creating accidental ambiguity.

## What the fix must not be

- **No phrase-specific rule.** Nothing like `order date -> placed_at`, and no
  list of phrases, words or entities that get special treatment.
- **Domain-agnostic.** It reasons only from generic concepts: semantic spans,
  overlapping phrases, relationship paths, the query root, the projection
  target, and binding validity. Nothing in it may know it is looking at a
  commerce schema.
- The standing rules still hold: exact ties are never accepted silently
  (011), a shared phrase needs distinguishing evidence in the sentence's words
  (019 §5), and a query must not answer a narrower or different question than
  the one asked (019 §1).

## When it is done

It gets its own gate, like every phase. At minimum, that gate:

- turns X11 into a pass through the generic mechanism, with no X11-specific
  code path;
- adds cases of the same *shape* on a config that is not commerce, so the fix
  is shown not to depend on the words in X11;
- re-runs the Phase 4 probes, the calibration sweep and the full query set,
  and reports every case whose outcome changed, with 0 silently wrong;
- mutation-tests the new cover and binding logic.
