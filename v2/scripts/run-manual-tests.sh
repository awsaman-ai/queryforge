#!/usr/bin/env bash
# Manual gate for every phase completed so far.
#
# Phase 0 (section 8): the existing QueryForge behaviour is unchanged, v2 is
# independently runnable, a simple config loads, an invalid one is rejected, and
# the CLI does not connect to a database.
#
# Phase 1 (section 9): the typed config model is deterministic and every invalid
# fixture is rejected for the EXPECTED reason, named by rule.
#
# Phase 1 redaction contract (section 16.10): config VALUES never appear in
# default output, and appear under --debug. Structure always appears.
#
# Phase 2 (section 10): the corpus is deterministic, the index builds, caches
# and refuses a stale cache, and retrieval is measurably semantic. The semantic
# checks are SKIPPED when the embedding venv is absent, so this script still
# runs in a fresh checkout.
#
# Phase 3 (section 11): phrase extraction produces stable spans across
# paraphrases, parses numbers and dates, and needs no config or model.
#
# Phase 4 (section 12): ranking decides accept / ambiguous / reject from
# similarity PLUS structural evidence and a top1-vs-top2 margin; it never
# silently guesses on the ambiguity set, and it materially outperforms raw
# vector top-1 there. The model-dependent checks SKIP without the venv.
#
# Phase 5 (section 13): accepted readings bind into a root entity, join paths
# from the schema graph, and conditions; the five required rejection cases
# refuse with stable codes. Model-dependent checks SKIP without the venv.
#
# Phase 6 (section 14): hand-written SemanticPlans compile to reviewed,
# byte-identical SQL; the compiler cannot emit an identifier outside the
# configured schema; unfaithful plans are refused with stable codes; the
# planner turns bindings into plans without dropping anything the sentence
# said. The --plan checks SKIP without the venv; --compile needs no model.
#
# Phase 7 (section 15): --query runs the whole pipeline — extraction,
# retrieval, ranking, binding, SemanticPlan, compiler — and prints the final
# query and parameters. The reviewed 77-case query set must reproduce its
# golden report exactly. Model-dependent checks SKIP without the venv.
#
# Phase 8 (section 16): security, ambiguity and hardening. Every adversarial
# case ends in a safe query or an explicit rejection — never unsafe SQL. The
# limits of section 16.7 are enforced as decision 021 defines them; SQL write
# keywords are refused as writes; every compiled query passes an output guard
# over the grammar's lexicon; config warnings report shared and unreachable
# phrases; the embedding worker makes no network connection; the pipeline is
# fuzzed. The reviewed 89-case adversarial set must reproduce its golden
# report exactly. Model-dependent checks SKIP without the venv.
#
# Phase 8 gate decisions (decision 022): a meaningful qualifier is never
# silently discarded, numbers are exact from the sentence to the parameters,
# SQL statement and comment syntax is refused at the input boundary, and the
# resource bounds (maxPhrases 256) are pinned by tests.
#
# Phase 9 (section 17): the benchmark harness measures what it claims to
# measure. Its arithmetic is unit-tested, the scale configs hit their targets
# exactly and preserve the reviewed schema, the report carries the header the
# gate requires, and no scenario ever spawned a model per request -- the
# pattern section 17 prohibits. The measured run itself is in
# benchmarks/reports/performance.md; this section checks the machinery and the
# report, not the wall-clock numbers, which differ on every machine.
#
# Usage: v2/scripts/run-manual-tests.sh
set -u

V2_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB_DIR="$(cd "$V2_DIR/.." && pwd)"

pass=0
fail=0

# check <name> <expected-exit> <command...>
# Runs the command, compares the exit status, and prints output only on failure
# so a passing run stays readable.
check() {
  local name="$1" want="$2"; shift 2
  local out status
  out="$("$@" 2>&1)"; status=$?
  if [ "$status" -eq "$want" ]; then
    printf '  PASS  %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s (exit %d, wanted %d)\n' "$name" "$status" "$want"
    printf '%s\n' "$out" | sed 's/^/          /'
    fail=$((fail + 1))
  fi
}

# check_output <name> <expected-substring> <command...>
check_output() {
  local name="$1" want="$2"; shift 2
  local out
  out="$("$@" 2>&1)"
  if printf '%s' "$out" | grep -q -- "$want"; then
    printf '  PASS  %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s (output does not contain %s)\n' "$name" "$want"
    printf '%s\n' "$out" | sed 's/^/          /'
    fail=$((fail + 1))
  fi
}

echo
echo "== 1. Production QueryForge is unchanged =="
check "library tests pass" 0 \
  env -C "$LIB_DIR" go test ./...
# The nested go.mod should prune v2 from the parent module's package patterns.
# If this fails, v2 has entered the library's build.
if env -C "$LIB_DIR" go list ./... 2>/dev/null | grep -q '/v2'; then
  echo "  FAIL  the library's package list now includes v2"
  fail=$((fail + 1))
else
  echo "  PASS  the library's package list excludes v2"
  pass=$((pass + 1))
fi

echo
echo "== 2. v2 is independently runnable =="
check "v2 builds"        0 env -C "$V2_DIR" go build ./...
check "v2 tests pass"    0 env -C "$V2_DIR" go test ./...
check "v2 is formatted"  0 env -C "$V2_DIR" sh -c '[ -z "$(gofmt -l .)" ]'
check "v2 vets clean"    0 env -C "$V2_DIR" go vet ./...
check_output "CLI reports its version" "phase-7" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --version

echo
echo "== 3. Config loading (Phase 0) =="
check_output "customers.json loads" "config OK" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/customers.json --validate-config
check_output "commerce.json loads" "entities=3 fields=15" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --validate-config
check_output "malformed JSON is rejected" "CONFIG_ERROR" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-malformed-json.json --validate-config

echo
echo "== 4. Every invalid fixture is rejected for the expected reason (Phase 1) =="
# "fixture:rule" — the rule name each fixture must report. Asserting the rule,
# not just that it failed, is what makes this a real check: a fixture that
# tripped some unrelated rule would otherwise look like a pass.
for pair in \
  "invalid-no-entities:no_entities" \
  "invalid-unnamed-entity:missing_entity_name" \
  "invalid-duplicate-entity:duplicate_entity" \
  "invalid-unknown-entity:unknown_entity" \
  "invalid-duplicate-field:duplicate_field" \
  "invalid-field-type:unsupported_field_type" \
  "invalid-operator-type:operator_type_mismatch" \
  "invalid-aggregation-type:aggregation_type_mismatch" \
  "invalid-unknown-operator:unknown_operator" \
  "invalid-identifier:invalid_identifier" \
  "invalid-value-alias:malformed_value_alias" \
  "invalid-enum:enum_values" \
  "invalid-missing-phrases:missing_phrases" \
  "invalid-relationship:unknown_relationship_target" \
  "invalid-relationship-types:relationship_type_mismatch" \
  "invalid-query-pattern:unknown_pattern_intent" \
  "invalid-security-limits:invalid_limit" \
; do
  fixture="${pair%%:*}"; rule="${pair##*:}"
  check_output "$fixture -> $rule" "$rule" \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config "./testdata/configs/$fixture.json" --validate-config
done

echo
echo "== 5. Config rejection is deterministic (Phase 1) =="
# Several validation rules walk Go maps, whose iteration order is randomised per
# run. Without explicit sorting this output would shuffle, and section 25's
# determinism requirement would fail before the semantic layers even exist.
det_first="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-value-alias.json --validate-config 2>&1)"
det_same=1
for _ in 1 2 3 4 5; do
  det_next="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-value-alias.json --validate-config 2>&1)"
  [ "$det_next" = "$det_first" ] || det_same=0
done
if [ "$det_same" -eq 1 ]; then
  echo "  PASS  six runs of a four-violation config produced identical output"
  pass=$((pass + 1))
else
  echo "  FAIL  rejection output varies between runs"
  fail=$((fail + 1))
fi

# A config author with several mistakes should see all of them in one run.
multi="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-relationship.json --validate-config 2>&1)"
if [ "$(printf '%s' "$multi" | grep -c '  - ')" -eq 3 ]; then
  echo "  PASS  all three problems reported in one run"
  pass=$((pass + 1))
else
  echo "  FAIL  expected three problems in one run"
  printf '%s\n' "$multi" | sed 's/^/          /'
  fail=$((fail + 1))
fi

echo
echo
echo "== 6. Redaction contract =="
# Config values (enum values, alias keys and targets) must not reach default
# output. Structure must, or the message cannot be acted on. Phase 2 builds the
# semantic corpus FROM these values, so the contract has to hold before it does.
redacted_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-value-alias.json --validate-config 2>&1)"
leak=0
for value in India india Nowhere disabled suspended; do
  if printf '%s' "$redacted_out" | grep -qw -- "$value"; then
    echo "  FAIL  default output leaks the config value '$value'"
    leak=1
  fi
done
if [ "$leak" -eq 0 ]; then
  echo "  PASS  default output leaks no config value"
  pass=$((pass + 1))
else
  fail=$((fail + 1))
fi

if printf '%s' "$redacted_out" | grep -q '<redacted>' && printf '%s' "$redacted_out" | grep -q 'customer.country'; then
  echo "  PASS  values redacted, structure retained"
  pass=$((pass + 1))
else
  echo "  FAIL  expected <redacted> values alongside structural field names"
  printf '%s\n' "$redacted_out" | sed 's/^/          /'
  fail=$((fail + 1))
fi

revealed_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-value-alias.json --validate-config --debug 2>&1)"
missing=0
for value in India Nowhere disabled suspended; do
  printf '%s' "$revealed_out" | grep -qw -- "$value" || { echo "  FAIL  --debug does not reveal '$value'"; missing=1; }
done
if [ "$missing" -eq 0 ]; then
  echo "  PASS  --debug reveals every withheld value"
  pass=$((pass + 1))
else
  fail=$((fail + 1))
fi

# A failure with nothing withheld must not grow an empty debug section.
quiet_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/invalid-relationship.json --validate-config --debug 2>&1)"
if printf '%s' "$quiet_out" | grep -q 'values revealed'; then
  echo "  FAIL  a config with nothing withheld printed a debug reveal section"
  fail=$((fail + 1))
else
  echo "  PASS  no debug reveal section when nothing was withheld"
  pass=$((pass + 1))
fi

echo
echo
echo "== 7. Semantic corpus and vector index (Phase 2) =="
# Mechanics first, with the deterministic hashing provider: no model download
# needed, and these checks are about plumbing rather than meaning.
check_output "corpus builds and is searchable" "rank" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --search "customer spending" --embedder deterministic --top-k 5

check_output "non-semantic provider warns loudly" "NOT semantic" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --search "x" --embedder deterministic

INDEX_TMP="$(mktemp -d)/commerce.index.json"
check_output "index builds and is written" "index written" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --build-index --index "$INDEX_TMP" --embedder deterministic
check_output "cached index is reused" "index loaded from" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --search "customer spending" --index "$INDEX_TMP" --embedder deterministic
# A stale index parses cleanly and would answer under names the config no longer
# has, which makes it more dangerous than a corrupt one.
check_output "stale index is refused" "stale" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/customers.json \
    --search "spending" --index "$INDEX_TMP" --embedder deterministic
rm -rf "$(dirname "$INDEX_TMP")"

# Corpus determinism across processes: the index is cached under a fingerprint
# of the corpus, so drift would invalidate the cache on every startup.
# Compares the SEMANTIC result only: the ranked list of (rank, score, kind, id,
# matched, redacted), plus the corpus shape and model identity.
#
# This projects the fields that must be stable rather than filtering out the
# ones that must not — an allowlist, not a denylist. Timing fields legitimately
# vary between runs, and an earlier version of this check listed embedMs and
# searchMs by name; a new timing field added in Phase 4 would have silently
# defeated it. Extracting what must match cannot be defeated that way.
retrieval_snapshot() {
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --search "customer" --embedder deterministic --top-k 20 --json 2>/dev/null \
    | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception as exc:
    print("UNPARSEABLE:", exc); raise SystemExit(0)
print(d.get("query"), d.get("strategy"), d.get("corpusEntries"), d.get("corpusVectors"))
print(json.dumps(d.get("model"), sort_keys=True))
for m in d.get("matches", []):
    print(m.get("rank"), repr(m.get("score")), m.get("kind"), m.get("id"),
          m.get("matched"), m.get("redacted"))
'
}
corpus_first="$(retrieval_snapshot)"
corpus_same=1
for _ in 1 2 3; do
  [ "$(retrieval_snapshot)" = "$corpus_first" ] || corpus_same=0
done
if [ "$corpus_same" -eq 1 ]; then
  echo "  PASS  corpus and retrieval are identical across processes"
  pass=$((pass + 1))
else
  echo "  FAIL  retrieval output varies between processes"
  fail=$((fail + 1))
fi

# Values are embedded — that is how "India" finds the country field — but the
# matched text must still redact.
value_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
  --search "India" --embedder deterministic --top-k 25 2>&1)"
if printf '%s' "$value_out" | grep -q 'VALUE_ALIAS'; then
  if printf '%s' "$value_out" | grep -q '<redacted>'; then
    echo "  PASS  retrieved config values are redacted"
    pass=$((pass + 1))
  else
    echo "  FAIL  a retrieved value was shown in default output"
    fail=$((fail + 1))
  fi
else
  echo "  SKIP  no value entry in the top 25 under the hashing embedder"
fi

echo
echo "== 8. Real semantic retrieval (Phase 2, needs the model) =="
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  no embedding venv; see embedding_worker/README.md"
else
  # The minimum bar for the whole experiment: a paraphrase sharing no tokens
  # with the field must still retrieve it.
  probe_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --search "money spent" --top-k 3 2>&1)"
  if printf '%s' "$probe_out" | head -6 | grep -q 'field:customer.total_spent'; then
    echo "  PASS  'money spent' retrieves customer.total_spent with no shared token"
    pass=$((pass + 1))
  else
    echo "  FAIL  the core paraphrase probe did not retrieve the expected field"
    printf '%s\n' "$probe_out" | sed 's/^/          /'
    fail=$((fail + 1))
  fi

  if printf '%s' "$probe_out" | grep -q 'semantic)'; then
    echo "  PASS  the real model reports itself semantic"
    pass=$((pass + 1))
  else
    echo "  FAIL  expected a semantic provider"
    fail=$((fail + 1))
  fi

  # The recorded accuracy from docs/phase-2-results.md. A regression here means
  # the corpus or the strategy changed under us.
  accuracy_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --probe ./testdata/queries/phase2-probes.tsv --strategy surface --top-k 5 2>&1)"
  # Both the numerator AND the denominator are parsed out. sed rather than
  # `grep -o ... | head -1`, because "Top-1: 27/33" begins with a digit in the
  # LABEL — the old version extracted the 1 from "Top-1" and reported a
  # catastrophic regression that did not exist.
  #
  # The denominator is read from the output rather than hardcoded, so adding a
  # probe to the TSV cannot silently turn this into a comparison against a
  # stale total.
  # The percentage is read from the report rather than recomputed here. Shell
  # integer division truncates, so recomputing turned 27/33 into 81% while the
  # report — and docs/phase-2-results.md — say 82%. One source of truth.
  top1="$(printf '%s' "$accuracy_out" | sed -n 's/.*Top-1: \([0-9][0-9]*\)\/\([0-9][0-9]*\) (\([0-9][0-9]*\)%).*/\1/p' | head -1)"
  scored="$(printf '%s' "$accuracy_out" | sed -n 's/.*Top-1: \([0-9][0-9]*\)\/\([0-9][0-9]*\) (\([0-9][0-9]*\)%).*/\2/p' | head -1)"
  pct1="$(printf '%s' "$accuracy_out" | sed -n 's/.*Top-1: \([0-9][0-9]*\)\/\([0-9][0-9]*\) (\([0-9][0-9]*\)%).*/\3/p' | head -1)"
  top3="$(printf '%s' "$accuracy_out" | sed -n 's/.*Top-3: \([0-9][0-9]*\)\/\([0-9][0-9]*\) (\([0-9][0-9]*\)%).*/\1/p' | head -1)"
  pct3="$(printf '%s' "$accuracy_out" | sed -n 's/.*Top-3: \([0-9][0-9]*\)\/\([0-9][0-9]*\) (\([0-9][0-9]*\)%).*/\3/p' | head -1)"

  if [ -z "$top1" ] || [ -z "$scored" ] || [ -z "$top3" ] || [ -z "$pct1" ] || [ -z "$pct3" ]; then
    echo "  FAIL  could not parse the probe accuracy out of the report"
    printf '%s\n' "$accuracy_out" | tail -5 | sed 's/^/          /'
    fail=$((fail + 1))
  else
    # Recorded in docs/phase-2-results.md: 27/33 Top-1, 30/33 Top-3.
    if [ "$top1" -ge 27 ] && [ "$top3" -ge 30 ]; then
      echo "  PASS  probe Top-1 $top1/$scored = ${pct1}%, Top-3 $top3/$scored = ${pct3}% (recorded: 27/33 = 82%, 30/33 = 91%)"
      pass=$((pass + 1))
    else
      echo "  FAIL  probe accuracy regressed: Top-1 $top1/$scored = ${pct1}%, Top-3 $top3/$scored = ${pct3}% (recorded: 27/33 = 82%, 30/33 = 91%)"
      fail=$((fail + 1))
    fi
  fi
fi

echo
echo
echo "== 9. Phrase extraction (Phase 3) =="
# No config, no model: extraction is config-independent by design.
check_output "analyze runs without a config" "candidate spans" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who spent more than 50000"

# The brief's own manual-gate sentences (section 11). Each must yield the span
# or value a later phase needs.
check_output "digits are parsed as a value" "50000" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who spent more than 50000"
check_output "spelled numbers are parsed" "spelled" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers spending over fifty thousand"
check_output "'last year' is a calendar period" "calendar year" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers created last year"
check_output "'last six months' is a rolling window" "rolling 6 month" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who registered in the last six months"
check_output "conjunctions are detected" "and" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers from India and orders above 100"

# The operator phrase must survive as its own span, or Phase 4 has nothing to
# bind the comparison to.
check_output "'more than' survives as a span" '"more than"' \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who spent more than 50000"

# The two Phase 2 retrieval misses that Phase 3 exists to fix: both failed
# because the whole phrase was embedded as one string.
for pair in "client location:location" "recent customer:recent"; do
  sentence="${pair%%:*}"; span="${pair##*:}"
  check_output "\"$sentence\" splits out \"$span\"" "\"$span\"" \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "$sentence"
done

# Extraction must be deterministic across processes.
span_first="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "show the top 10 customers from India who spent more than 50000 last year" --json 2>/dev/null)"
span_same=1
for _ in 1 2 3; do
  span_next="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "show the top 10 customers from India who spent more than 50000 last year" --json 2>/dev/null)"
  [ "$span_next" = "$span_first" ] || span_same=0
done
if [ "$span_same" -eq 1 ]; then
  echo "  PASS  extraction is identical across processes"
  pass=$((pass + 1))
else
  echo "  FAIL  extraction varies between processes"
  fail=$((fail + 1))
fi

echo
echo
echo "== 10. Candidate ranking (Phase 4) =="
# Mechanics with the hashing embedder: no model needed.
check_output "--rank reports a reading and its policy" "minScore 0.70, minMargin 0.10" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
    --rank "orders with amount over 100" --embedder deterministic
check_output "an impossible threshold is refused" "INVALID_INPUT" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
    --rank "amount" --embedder deterministic --min-score 0
check_output "hand-set thresholds are announced" "OVERRIDDEN" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
    --rank "amount" --embedder deterministic --min-margin 0.2

# Redaction through ranking. "Freedon" is a misspelling: it retrieves the
# Freedonia alias WITHOUT the user having typed the value, so any rendering of
# that alias — in an id, a surface, or an error — is a leak. A sentence that
# typed its own value could never show one.
rank_leak_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
  --rank "customers from Freedon" --embedder deterministic 2>&1)
$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
  --rank "customers from Freedon" --embedder deterministic --json 2>&1)"
rank_leak=0
for value in Freedonia FD_QX Sylvania SY_QX zorbified lapsed_qx bounced declined_qx; do
  if printf '%s' "$rank_leak_out" | grep -qw -- "$value"; then
    echo "  FAIL  ranking output leaks the config value '$value'"
    rank_leak=1
  fi
done
if [ "$rank_leak" -eq 0 ]; then
  echo "  PASS  ranking output (text and JSON) leaks no config value"
  pass=$((pass + 1))
else
  fail=$((fail + 1))
fi
# Checked in --json --debug, where targets are never truncated: the text
# table cuts ids to 34 columns, and "alias:Free…" is not evidence of a reveal.
check_output "--debug reveals the value in ranking" '"target": "value:customer.country:alias:Freedonia"' \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
    --rank "customers from Freedon" --embedder deterministic --debug --json

# Phase 2 regression found in Phase 4: --search printed value-entry ids
# verbatim ("value:customer.country:alias:India") next to a <redacted> column.
search_ids="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
  --search "country" --embedder deterministic --top-k 55 2>&1)"
if printf '%s' "$search_ids" | grep -q 'alias:India\|alias:United\|enum:suspended'; then
  echo "  FAIL  --search prints value-entry ids unredacted"
  fail=$((fail + 1))
else
  echo "  PASS  --search redacts value-entry ids (Phase 2 leak, fixed in Phase 4)"
  pass=$((pass + 1))
fi

# Determinism across processes. Projects the decision fields (allowlist, as
# section 7 does) so a timing field cannot defeat the comparison.
rank_snapshot() {
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
    --rank "orders and payments with amount over 5" --embedder deterministic --json --debug 2>/dev/null \
    | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception as exc:
    print("UNPARSEABLE:", exc); raise SystemExit(0)
print(d.get("verdict"), json.dumps(d.get("policy"), sort_keys=True))
for s in d.get("allSpans", []):
    print(s["span"], s["decision"], repr(s["margin"]), repr(s["confidence"]))
    for c in s.get("candidates", []):
        print("   ", c["target"], repr(c["similarity"]), repr(c["score"]), c.get("vetoed"))
'
}
rank_first="$(rank_snapshot)"
rank_same=1
for _ in 1 2 3; do
  [ "$(rank_snapshot)" = "$rank_first" ] || rank_same=0
done
if [ "$rank_same" -eq 1 ] && [ -n "$rank_first" ]; then
  echo "  PASS  ranking is identical across processes"
  pass=$((pass + 1))
else
  echo "  FAIL  ranking output varies between processes"
  fail=$((fail + 1))
fi

echo
echo "== 11. Ranking against the real model (Phase 4, needs the model) =="
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  no embedding venv; see embedding_worker/README.md"
else
  # The brief's manual gate (section 12): the five ambiguous words, each on
  # its own, must NOT be silently resolved. AMBIGUOUS_MATCH, or a refusal.
  for word in amount value date status name; do
    word_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json --rank "$word" 2>&1)"
    if printf '%s' "$word_out" | grep -q 'AMBIGUOUS_MATCH\|NO_SEMANTIC_MATCH\|LOW_CONFIDENCE'; then
      verdict_code="$(printf '%s' "$word_out" | sed -n 's/^verdict: *//p')"
      echo "  PASS  \"$word\" alone is not silently resolved ($verdict_code)"
      pass=$((pass + 1))
    else
      echo "  FAIL  \"$word\" was silently resolved"
      printf '%s\n' "$word_out" | sed 's/^/          /'
      fail=$((fail + 1))
    fi
  done

  # ...and the same words, decided by deterministic evidence in a sentence.
  for triple in \
    "orders with amount over 100|amount|field:order.amount" \
    "payments with amount over 100|amount|field:payment.amount" \
    "status of the payment|status|field:payment.status" \
    "the 5 highest orders|highest|pattern:top_n" \
  ; do
    sentence="${triple%%|*}"; rest="${triple#*|}"; phrase="${rest%%|*}"; target="${rest#*|}"
    decided="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json --rank "$sentence" 2>&1)"
    if printf '%s' "$decided" | grep -E "^  \"$phrase\" +accept +$target" >/dev/null; then
      echo "  PASS  \"$sentence\": \"$phrase\" -> $target"
      pass=$((pass + 1))
    else
      echo "  FAIL  \"$sentence\": expected \"$phrase\" accepted as $target"
      printf '%s\n' "$decided" | sed 's/^/          /'
      fail=$((fail + 1))
    fi
  done

  # Decision 012 (Phase 4 gate): "amount" is contextual. Ambiguous alone when
  # two fields are named amount; decided by an exact synonym or by entity +
  # value + operator evidence; accepted alone when only one field has the name.
  check_output "decision 012: 'amount' alone is ambiguous (two amount fields)" "verdict:  AMBIGUOUS_MATCH" \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json --rank "amount"
  for triple in \
    "ambiguity|order amount|order amount|field:order.amount" \
    "ambiguity|payment amount|payment amount|field:payment.amount" \
    "ambiguity|payments with amount over 100|amount|field:payment.amount" \
    "commerce|amount|amount|field:order.amount" \
  ; do
    cfg="${triple%%|*}"; rest="${triple#*|}"; sentence="${rest%%|*}"; rest="${rest#*|}"
    phrase="${rest%%|*}"; target="${rest#*|}"
    decided="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config "./testdata/configs/$cfg.json" --rank "$sentence" 2>&1)"
    if printf '%s' "$decided" | grep -E "^  \"$phrase\" +accept +$target" >/dev/null; then
      echo "  PASS  decision 012 ($cfg): \"$sentence\" -> $target"
      pass=$((pass + 1))
    else
      echo "  FAIL  decision 012 ($cfg): \"$sentence\" should accept \"$phrase\" as $target"
      printf '%s\n' "$decided" | sed 's/^/          /'
      fail=$((fail + 1))
    fi
  done

  # Phase 2 regressions, asked of the ranker.
  check_output "Phase 2 miss: 'placed an order' reads the verb as the edge" "accept     relationship:customer_orders" \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --rank "placed an order"
  check_output "Phase 2 tie: 'highest' alone is ambiguous, not decided by id" "vs       pattern:top_n" \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --rank "highest"

  # The exit criterion: ranking materially outperforms raw vector top-1 on the
  # ambiguity set, and never silently guesses. Recorded in
  # docs/phase-4-results.md (final, after the decision-012 rows): ambiguity
  # 33/34 vs raw 20/34, commerce 17/18 vs raw 13/18, zero silently wrong.
  for set in "ambiguity:33:34" "commerce:17:18"; do
    name="${set%%:*}"; rest="${set#*:}"; recorded="${rest%%:*}"; total="${rest#*:}"
    probe="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config "./testdata/configs/$name.json" \
      --rank-probe "./testdata/queries/phase4-$name.tsv" 2>&1)"
    line="$(printf '%s' "$probe" | grep '^RANKPROBE')"
    raw="$(printf '%s' "$line" | sed -n 's/.* raw=\([0-9]*\) .*/\1/p')"
    ranked="$(printf '%s' "$line" | sed -n 's/.* ranked=\([0-9]*\) .*/\1/p')"
    scored="$(printf '%s' "$line" | sed -n 's/.* scored=\([0-9]*\) .*/\1/p')"
    raw_wrong="$(printf '%s' "$line" | sed -n 's/.* raw_wrong=\([0-9]*\) .*/\1/p')"
    wrong="$(printf '%s' "$line" | sed -n 's/.* ranked_wrong=\([0-9]*\).*/\1/p')"
    if [ -z "$ranked" ] || [ -z "$raw" ] || [ -z "$wrong" ] || [ -z "$scored" ]; then
      echo "  FAIL  could not parse the $name rank probe"
      printf '%s\n' "$probe" | tail -8 | sed 's/^/          /'
      fail=$((fail + 1))
      continue
    fi
    if [ "$wrong" -eq 0 ] && [ "$ranked" -ge "$recorded" ] && [ "$scored" -eq "$total" ] && [ "$ranked" -gt "$raw" ]; then
      echo "  PASS  $name set: ranked $ranked/$scored vs raw top-1 $raw/$scored; silently wrong: ranked 0, raw $raw_wrong"
      pass=$((pass + 1))
    else
      echo "  FAIL  $name set: ranked $ranked/$scored (recorded $recorded/$total), raw $raw/$scored, ranked silently wrong $wrong"
      fail=$((fail + 1))
    fi
  done
fi

echo
echo
echo "== 12. Semantic binding + schema graph (Phase 5) =="
check_output "--bind reports a binding section" "binding:" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --bind "customers from India" --embedder deterministic

# Redaction through binding. "Freedon Zorblatt" is refused (unexplained word)
# and its reading contains the Freedonia alias the user did not type; a
# successful binding would print no corpus id at all, so the check needs a
# refusal to have teeth (found by mutation-testing the Phase 5 sweep).
bind_leak_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
  --bind "customers from Freedon Zorblatt" --embedder deterministic 2>&1)
$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/multipath.json \
  --bind "orders shipped to customers in Ruritani" --embedder deterministic 2>&1)
$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/multipath.json \
  --bind "orders shipped to customers in Ruritani" --embedder deterministic --json 2>&1)"
bind_leak=0
for value in Freedonia FD_QX Ruritania RU_QX; do
  if printf '%s' "$bind_leak_out" | grep -qw -- "$value"; then
    echo "  FAIL  binding output leaks the config value '$value'"
    bind_leak=1
  fi
done
if [ "$bind_leak" -eq 0 ]; then
  echo "  PASS  binding output (text, JSON, refusal) leaks no config value"
  pass=$((pass + 1))
else
  fail=$((fail + 1))
fi

# Decision 013 draws the user-text line by AUDIENCE: the stdout report may echo
# the sentence (it is diagnostic), the error on stderr may not. So this check
# reads stderr ONLY. (Its first version grepped both streams together and
# failed on the report's legitimate echo of the query.)
bind_err_only="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
  --bind "customers from Freedon Zorblatt" --embedder deterministic 2>&1 >/dev/null)"
if printf '%s' "$bind_err_only" | grep -qw -- "Zorblatt"; then
  echo "  FAIL  the binding error quotes the user's word by default"
  fail=$((fail + 1))
elif printf '%s' "$bind_err_only" | grep -q 'tokens\['; then
  echo "  PASS  the binding error locates the phrase by position, not by quoting it"
  pass=$((pass + 1))
else
  echo "  FAIL  expected a refusal locating the phrase by token position"
  printf '%s\n' "$bind_err_only" | sed 's/^/          /'
  fail=$((fail + 1))
fi

bind_snapshot() {
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --bind "customers from India who bought laptops" --embedder deterministic --json --debug 2>/dev/null
}
bind_first="$(bind_snapshot)"
bind_same=1
for _ in 1 2 3; do
  [ "$(bind_snapshot)" = "$bind_first" ] || bind_same=0
done
if [ "$bind_same" -eq 1 ] && [ -n "$bind_first" ]; then
  echo "  PASS  binding is identical across processes"
  pass=$((pass + 1))
else
  echo "  FAIL  binding output varies between processes"
  fail=$((fail + 1))
fi

echo
echo "== 13. Binding against the real model (Phase 5, needs the model) =="
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  no embedding venv; see embedding_worker/README.md"
else
  # bind_expect <label> <config> <sentence> <substring>...: every substring
  # must appear in the --bind output. --debug so stored values are visible
  # where a check needs them.
  bind_expect() {
    local label="$1" cfg="$2" sentence="$3"; shift 3
    local out missing=""
    out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config "./testdata/configs/$cfg.json" --bind "$sentence" --debug 2>&1)"
    for want in "$@"; do
      printf '%s' "$out" | grep -qF -- "$want" || missing="$missing [$want]"
    done
    if [ -z "$missing" ]; then
      echo "  PASS  $label"
      pass=$((pass + 1))
    else
      echo "  FAIL  $label — missing:$missing"
      printf '%s\n' "$out" | sed 's/^/          /'
      fail=$((fail + 1))
    fi
  }

  # The brief's four gate sentences (section 13).
  bind_expect "customers from India" commerce "customers from India" \
    "root: customer" 'customer.country eq "IN" (alias)'
  bind_expect "customers who bought laptops: customer → order → product" commerce "customers who bought laptops" \
    "customer -[customer_orders]-> order -[order_product]-> product" 'product.category eq "laptop" (alias)'
  bind_expect "customers who ordered more than 3 times: a count through the relationship" commerce \
    "customers who ordered more than 3 times" "count(customer_orders) gt 3"
  bind_expect "customers whose order amount is above 10000: a condition on a joined entity" commerce \
    "customers whose order amount is above 10000" "root: customer" \
    "order.amount gt 10000  via customer -[customer_orders]-> order"

  # Multi-condition, multi-entity.
  bind_expect "two conditions joined by AND" commerce "customers from India who spent more than 50000" \
    'customer.country eq "IN"' "AND  customer.total_spent gt 50000"
  bind_expect "the verb chooses between two join paths" multipath "orders shipped to customers in Ruritania" \
    "order -[order_shipping_customer]-> customer"

  # The five required rejection cases.
  bind_expect "reject: candidates cannot be connected" multipath "customers with warehouse capacity over 100" \
    "RELATIONSHIP_NOT_FOUND"
  bind_expect "reject: equally plausible join paths" multipath "orders from customers in Ruritania" \
    "AMBIGUOUS_MATCH" "order_billing_customer" "order_shipping_customer"
  bind_expect "reject: a field incompatible with the value's type" commerce "customer name over 5" \
    "UNSUPPORTED_QUERY" "is a string field"
  bind_expect "reject: an operator outside the field's allowlist" commerce "customer spending contains 5" \
    "UNSUPPORTED_QUERY" "does not allow"
  bind_expect "reject: aggregation invalid for a field type" commerce "average customer name" \
    "UNSUPPORTED_QUERY"
  bind_expect "reject: a value that cannot be resolved safely" commerce "customers with status cancelled" \
    "VALUE_RESOLUTION_FAILED"
  bind_expect "reject: a time window with no period" commerce "recent customers" \
    "VALUE_RESOLUTION_FAILED"
  bind_expect "reject: an unexplained word is never dropped" commerce "customers from Atlantis" \
    "UNSUPPORTED_QUERY"
fi

echo
echo "== 14. Deterministic compiler (Phase 6, no model) =="
# compile <config> <plan> [flags...]: the --compile CLI on a fixture plan.
compile() {
  local cfg="$1" plan="$2"; shift 2
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config "./testdata/configs/$cfg.json" \
    --compile "./testdata/plans/$plan.json" "$@" 2>&1
}
# Each accepted plan against its reviewed golden (SQL + revealed params).
for pair in brief-example:commerce joins-and-subqueries:commerce exists-and-count:commerce \
            group-having:commerce multipath-two-joins:multipath; do
  plan="${pair%%:*}" cfg="${pair##*:}"
  got="$(compile "$cfg" "$plan" --debug)"
  if [ "$got" = "$(cat "$V2_DIR/testdata/plans/$plan.golden")" ]; then
    echo "  PASS  $plan compiles to its reviewed golden SQL"
    pass=$((pass + 1))
  else
    echo "  FAIL  $plan differs from testdata/plans/$plan.golden"
    printf '%s\n' "$got" | sed 's/^/          /'
    fail=$((fail + 1))
  fi
done

# Section 14's gate: the same plan gives byte-for-byte the same SQL, across
# separate processes.
same=1
for plan in brief-example exists-and-count group-having joins-and-subqueries; do
  first="$(compile commerce "$plan" --debug --json)"
  for _ in 1 2 3; do
    [ "$(compile commerce "$plan" --debug --json)" = "$first" ] || same=0
  done
done
if [ "$same" -eq 1 ]; then
  echo "  PASS  every plan compiles byte-identically across processes"
  pass=$((pass + 1))
else
  echo "  FAIL  compiled SQL varies between processes"
  fail=$((fail + 1))
fi

# The exit criterion: no identifier outside the configured schema. The golden
# plans, a hostile-value sweep, an unvalidated registry, and a fuzz corpus.
check "compiler emits only configured identifiers (golden, hostile values, unvalidated registry)" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestGoldenOutputsUseOnlyConfiguredIdentifiers|TestHostileValuesStayParameters|TestUnvalidatedRegistryCannotInjectIdentifiers|TestUnconfiguredNamesAreRefused' ./internal/compiler
check "compiler fuzz: 20s of arbitrary names, paths and values" 0 \
  env -C "$V2_DIR" go test -run XXX -fuzz FuzzCompileOnlyEmitsConfiguredIdentifiers -fuzztime 20s ./internal/compiler

# Unfaithful and malformed plans are refused, each with its code.
for pair in refuse-identifier-injection:SECURITY_REJECTED refuse-unconfigured-join:SECURITY_REJECTED \
            refuse-same-row-ambiguity:AMBIGUOUS_MATCH refuse-fanout-sum:UNSUPPORTED_QUERY \
            refuse-unknown-key:INVALID_INPUT; do
  check_output "refused: ${pair%%:*}" "${pair##*:}" compile commerce "${pair%%:*}"
done
refused_inj="$(compile commerce refuse-identifier-injection)"
if printf '%s' "$refused_inj" | grep -q "DROP"; then
  echo "  FAIL  the refusal echoes the injected identifier"
  fail=$((fail + 1))
else
  echo "  PASS  the refusal does not echo the injected identifier"
  pass=$((pass + 1))
fi

# Redaction: the string parameter is a config value (alias target "IN").
if compile commerce brief-example | grep -q '"IN"'; then
  echo "  FAIL  --compile shows a string parameter without --debug"
  fail=$((fail + 1))
elif compile commerce brief-example --debug | grep -q '1 = "IN" (string)'; then
  echo "  PASS  string parameters are redacted by default and shown under --debug"
  pass=$((pass + 1))
else
  echo "  FAIL  --debug does not reveal the string parameter"
  fail=$((fail + 1))
fi

check "planner and compiler unit tests (rules, accounting tripwire, mutations' targets)" 0 \
  env -C "$V2_DIR" go test -count=1 ./internal/plan ./internal/compiler
check_output "--plan stops at the plan: no SQL before Phase 7" "clock:" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --plan "orders last month" --embedder deterministic --now 2026-09-21T12:00:00Z
if env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --plan "orders last month" --embedder deterministic --now 2026-09-21T12:00:00Z 2>&1 | grep -q SELECT; then
  echo "  FAIL  --plan printed SQL"
  fail=$((fail + 1))
else
  echo "  PASS  --plan printed no SQL"
  pass=$((pass + 1))
fi

echo
echo "== 15. Planning against the real model (Phase 6, needs the model) =="
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  no embedding venv; see embedding_worker/README.md"
else
  # plan_expect <label> <sentence> <substring>...: --plan on commerce with a
  # pinned clock, so date ranges are reproducible.
  plan_expect() {
    local label="$1" sentence="$2"; shift 2
    local out missing=""
    out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --plan "$sentence" \
      --now 2026-09-21T12:00:00Z --debug 2>&1)"
    for want in "$@"; do
      printf '%s' "$out" | grep -qF -- "$want" || missing="$missing [$want]"
    done
    if [ -z "$missing" ]; then
      echo "  PASS  $label"
      pass=$((pass + 1))
    else
      echo "  FAIL  $label — missing:$missing"
      printf '%s\n' "$out" | sed 's/^/          /'
      fail=$((fail + 1))
    fi
  }
  plan_expect "customers from India" "customers from India" 'where: customer.country eq "IN"'
  plan_expect "customers who bought laptops: the path survives into the plan" "customers who bought laptops" \
    'where: product.category via customer_orders → order_product eq "laptop"'
  plan_expect "a count, with the unit note carried, not dropped" "customers who ordered more than 3 times" \
    "where: count(customer_orders) gt 3" "note: a unit word after the count"
  plan_expect "top 5: direction from config" "top 5 customers by spending" "order by: customer.total_spent desc" "limit: 5"
  plan_expect "bottom 3: direction from config" "bottom 3 customers by spending" "order by: customer.total_spent asc" "limit: 3"
  plan_expect "last month resolves half-open against the clock" "orders last month" \
    "order.placed_at in [2026-08-01T00:00:00Z, 2026-09-01T00:00:00Z)"
  plan_expect "a currency the config cannot interpret is refused" 'customers who spent more than $50000' \
    "VALUE_RESOLUTION_FAILED" "carries a currency"
  plan_expect "an unexplained word is still refused before planning" "customers from Atlantis" \
    "binding:  REFUSED (UNSUPPORTED_QUERY)"
fi

echo
echo "== 16. End-to-end pipeline (Phase 7, no model) =="
check "pipeline tests: brief examples, stages, telemetry redaction, 32 concurrent requests (-race)" 0 \
  env -C "$V2_DIR" go test -race -count=1 ./internal/pipeline
check "CLI contract: SQL only on success, JSON shape, explain, telemetry only under --debug" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestQueryIsDerivedOrRefused|TestQueryJSONContract|TestQueryTelemetryIsStructuredAndRedacted|TestQueryExplainShowsTheStages|TestQueryRefusalDoesNotQuoteTheUser|TestE2ERefusesAMismatchedConfig' ./cmd/qf-v2
if env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "show customers from India" \
    --embedder deterministic 2>&1 | grep -q NOT_IMPLEMENTED; then
  echo "  FAIL  --query still reports NOT_IMPLEMENTED"
  fail=$((fail + 1))
else
  echo "  PASS  --query runs the pipeline; NOT_IMPLEMENTED is gone"
  pass=$((pass + 1))
fi
cases="$(grep -c '"id":' "$V2_DIR/testdata/queries/phase7-e2e.json")"
if [ "$cases" -ge 50 ]; then
  echo "  PASS  the reviewed query set has $cases cases (section 15 asks for at least 50)"
  pass=$((pass + 1))
else
  echo "  FAIL  the reviewed query set has only $cases cases"
  fail=$((fail + 1))
fi

echo
echo "== 17. End to end against the real model (Phase 7, needs the model) =="
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  no embedding venv; see embedding_worker/README.md"
else
  q() {
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "$1" \
      --now 2026-09-21T12:00:00Z 2>&1
  }
  # query_expect <label> <sentence> <substring>...
  query_expect() {
    local label="$1" sentence="$2"; shift 2
    local out missing=""
    out="$(q "$sentence")"
    for want in "$@"; do
      printf '%s' "$out" | grep -qF -- "$want" || missing="$missing [$want]"
    done
    if [ -z "$missing" ]; then
      echo "  PASS  $label"
      pass=$((pass + 1))
    else
      echo "  FAIL  $label — missing:$missing"
      printf '%s\n' "$out" | sed 's/^/          /'
      fail=$((fail + 1))
    fi
  }
  query_expect "brief example 1: customers from India" "Show customers from India" \
    'FROM "customers" AS "t0"' 'WHERE "t0"."country" = $1;' '1 = "IN" (string)'
  query_expect "brief example 2: spent more than 50000" "Show customers who spent more than 50000" \
    'WHERE "t0"."total_spent" > $1;' '1 = 50000 (number)'
  query_expect "brief example 3 as worded: refused — the top-N metric must be named (019 §2)" \
    "Show the top 10 customers from India who spent more than 50000 last year" "UNSUPPORTED_QUERY" "names no field to rank them by"
  query_expect "brief example 3 with the metric named: the brief's shape" "Show the top 10 customers by spending from India" \
    'WHERE "t0"."country" = $1' 'ORDER BY "t0"."total_spent" DESC NULLS LAST' 'LIMIT 10;'
  query_expect "a join end to end: EXISTS, never a row-multiplying JOIN" "customers who bought laptops" \
    'WHERE EXISTS (' '"category" = $1' '1 = "laptop" (string)'
  query_expect "contradictory AND refused, not an empty answer (finding M06)" "customers from India and United States" \
    "AMBIGUOUS_MATCH" "two different values at once"
  # Decision 019 (Phase 7 gate).
  query_expect "019 §1: a value needs literal evidence — 'living' never becomes status 'live'" \
    "customers living in India" "UNSUPPORTED_QUERY"
  if q "customers living in India" | grep -q '"status"'; then
    echo "  FAIL  a value was bound on similarity alone"
    fail=$((fail + 1))
  else
    echo "  PASS  no status filter from a similar-looking word"
    pass=$((pass + 1))
  fi
  query_expect "019 §1: the literal alias still resolves" "live customers from India" \
    '"t0"."status" = $1' '1 = "active" (string)'
  query_expect "019 §5: a word the config uses for two fields is not settled by context" \
    "show customer value" "AMBIGUOUS_MATCH" "nothing in the sentence distinguishes them"
  # 019 §5 at the re-gate (finding X02): a shared phrase needs distinguishing
  # evidence in the sentence's words. Entity context and a 0.015 similarity
  # difference are not evidence.
  query_expect "019 §5: 'show customer date' refused — 'date' is shared, 'customer' does not distinguish" \
    "show customer date" "AMBIGUOUS_MATCH" "nothing in the sentence distinguishes them" \
    "field:customer.created_at" "field:order.placed_at"
  if q "show customer date" | grep -q "margin"; then
    echo "  FAIL  the shared-phrase refusal reports a margin shortfall that did not happen"
    fail=$((fail + 1))
  else
    echo "  PASS  the shared-phrase refusal gives its own reason, not a margin"
    pass=$((pass + 1))
  fi
  query_expect "019 §5: a configured phrase in the span settles it — 'show customer creation date'" \
    "show customer creation date" 'SELECT "t0"."created_at"' 'FROM "customers" AS "t0";'
  query_expect "019 §5: a configured phrase spelled out, not contiguously — 'orders by date'" \
    "orders by date" 'SELECT "t0"."placed_at"' 'FROM "orders" AS "t0";'
  # ambiguity.json configures "customer date" on customer.created_at: the
  # explicit alias half of the rule, on a real fixture.
  alias_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
    --query "show customer date" --now 2026-09-21T12:00:00Z 2>&1)"
  if printf '%s' "$alias_out" | grep -qF 'SELECT "t0"."created_at"' && printf '%s' "$alias_out" | grep -qF 'FROM "customers" AS "t0";'; then
    echo "  PASS  019 §5: an explicit config phrase ('customer date' on created_at) is accepted"
    pass=$((pass + 1))
  else
    echo "  FAIL  019 §5: the configured phrase 'customer date' did not resolve"
    printf '%s\n' "$alias_out" | sed 's/^/          /'
    fail=$((fail + 1))
  fi
  query_expect "019 §2: the top-N metric is never inferred from a comparison" \
    "top 10 customers who spent more than 50000" "UNSUPPORTED_QUERY"

  # The reviewed query set: byte-identical to its golden report.
  e2e_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --e2e ./testdata/queries/phase7-e2e.json 2>/dev/null)"
  if [ "$e2e_out" = "$(cat "$V2_DIR/testdata/queries/phase7-e2e.golden")" ]; then
    echo "  PASS  the $cases-case query set reproduces its reviewed golden report ($(printf '%s' "$e2e_out" | grep -o '[0-9]*/[0-9]* pass'))"
    pass=$((pass + 1))
  else
    echo "  FAIL  the query set's report differs from testdata/queries/phase7-e2e.golden"
    diff <(printf '%s\n' "$e2e_out") "$V2_DIR/testdata/queries/phase7-e2e.golden" | head -40 | sed 's/^/          /'
    fail=$((fail + 1))
  fi
  # Determinism across processes, end to end.
  if [ "$(q "customers from India who bought laptops")" = "$(q "customers from India who bought laptops")" ]; then
    echo "  PASS  --query is identical across processes"
    pass=$((pass + 1))
  else
    echo "  FAIL  --query output varies between processes"
    fail=$((fail + 1))
  fi
fi

echo
echo "== 18. Scope and honesty =="
check_output "non-postgres backend is rejected" "INVALID_INPUT" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/customers.json --query "show customers" --backend mysql

echo
echo "== 19. No database access =="
# The guard test already fails the build on a driver import; this repeats the
# check at the source level so the gate does not depend on one test's denylist.
#
# internal/isolation is excluded because that guard's denylist necessarily spells
# out the very strings being searched for — grepping it would always "find" a
# driver. Its own test is what covers that directory.
if grep -rIl --include='*.go' -E '"database/sql"|sql\.Open|postgres://|mysql://|mongodb://' \
     --exclude-dir=isolation "$V2_DIR" >/dev/null 2>&1; then
  echo "  FAIL  found database access in v2 sources"
  fail=$((fail + 1))
else
  echo "  PASS  no database driver, connection string, or sql.Open in v2"
  pass=$((pass + 1))
fi

echo
echo "== 20. Security and hardening (Phase 8, no model) =="
check "Phase 8 unit tests: limits, output guard, read-only rule, config warnings, qualifier rule (race)" 0 \
  env -C "$V2_DIR" go test -race -count=1 ./internal/readonly ./internal/configlint ./internal/compiler ./internal/pipeline ./internal/ranking
check "e2e runner: REFUSED expectation and visible control characters" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestE2EExpectRefused|TestReportShowsInvisibleCharacters' ./cmd/qf-v2
# Decisions 016 and 019 §4: warnings, never errors — the config still loads.
check_output "config warnings: phrases shared across kinds (019 §4)" "phrase_shared_across_kinds" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --validate-config
check_output "config warnings: phrases that never form ('one of', 016)" 'the phrase "one of" on the operator in contains "one"' \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --validate-config
check "a config with warnings still loads (exit 0)" 0 \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --validate-config
clean_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/customers.json --validate-config 2>&1)"
if printf '%s' "$clean_out" | grep -q "warning"; then
  echo "  FAIL  a config with no collisions reports warnings"
  printf '%s\n' "$clean_out" | sed 's/^/          /'
  fail=$((fail + 1))
else
  echo "  PASS  a config with no collisions reports none"
  pass=$((pass + 1))
fi
# Section 16.1, model-free: the rule runs on the reading, whatever produced it.
check_output "16.1: 'Delete all customers' is refused as a write" "write keyword DELETE; the engine only reads" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --embedder deterministic --query "Delete all customers"
# Without a ";" since the Phase 8 gate: the separator is now refused one stage
# earlier, at the input boundary (section 22), which would hide this rule.
check_output "16.1: a stacked DROP is refused as a write, not as 'matches nothing'" "write keyword DROP" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --embedder deterministic --query "SELECT * FROM users DROP TABLE users"
# Section 16.7: a retrieval depth over maxCandidates fails at startup.
check_output "16.7: --top-k over security.maxCandidates is a startup CONFIG_ERROR" "over security.maxCandidates" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --embedder deterministic --top-k 300 --query "customers from India"
check "fuzz: the whole pipeline on arbitrary input for 30s (no panic, answer or explicit refusal)" 0 \
  env -C "$V2_DIR" go test -run XXX -fuzz FuzzPipeline -fuzztime 30s ./internal/pipeline

echo
echo "== 21. Security against the real model (Phase 8, needs the model) =="
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  no embedding venv; see embedding_worker/README.md"
else
  check "16.11: the embedding worker makes no network connection (audit hook, self-tested)" 0 \
    env -C "$V2_DIR" go test -count=1 -run 'TestNetworkCheckCanSeeAConnection|TestWorkerMakesNoNetworkConnections' ./internal/embedding
  adv_cases="$(grep -c '"id":' "$V2_DIR/testdata/queries/phase8-adversarial.json")"
  adv_out="$(env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
    --e2e ./testdata/queries/phase8-adversarial.json 2>/dev/null)"
  if [ "$adv_out" = "$(cat "$V2_DIR/testdata/queries/phase8-adversarial.golden")" ]; then
    echo "  PASS  the $adv_cases-case adversarial set reproduces its reviewed golden report ($(printf '%s' "$adv_out" | grep -o '[0-9]*/[0-9]* pass'))"
    pass=$((pass + 1))
  else
    echo "  FAIL  the adversarial set's report differs from testdata/queries/phase8-adversarial.golden"
    diff <(printf '%s\n' "$adv_out") "$V2_DIR/testdata/queries/phase8-adversarial.golden" | head -40 | sed 's/^/          /'
    fail=$((fail + 1))
  fi
  # The gate's own criterion, read off the report: every case is an answer or
  # an explicit refusal, and every answer passed the output guard (a guard
  # failure is a refusal, so no row can be a query that failed it).
  if printf '%s' "$adv_out" | grep -q 'REFUSED INTERNAL_ERROR\|REFUSED CONFIG_ERROR'; then
    echo "  FAIL  an adversarial case ended in an internal or config error"
    fail=$((fail + 1))
  else
    echo "  PASS  every adversarial case ends in a safe query or an explicit rejection"
    pass=$((pass + 1))
  fi
fi

echo
echo "== 22. Phase 8 gate decisions (decision 022) =="
# §1 semantic completeness, §2 exact numbers, §3 the input boundary, §4 the
# resource bounds, §5 explicit-vs-similarity, §6 the opening request verb. The
# model-dependent evidence is the 93-case set in section 21 (groups P and G);
# these are the model-free checks, then two sentences that need the model.
check "§1-§3 unit tests: exact decimals, extraction, completeness, boundary, precision end to end, calendar range, api (race)" 0 \
  env -C "$V2_DIR" go test -race -count=1 ./internal/numeric ./internal/nlp ./internal/binding ./internal/inputguard ./internal/pipeline ./internal/plan ./api
check_output "§2: a 23-digit literal is extracted exactly (was 1e23)" "-> 99999999999999999999999" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who spent more than 99999999999999999999999"
check_output "§2: a minus sign is part of the number (was dropped)" '"-5"                     -> -5' \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who spent more than -5"
check_output "§2: a malformed number is reported, not read as its digits" "malformed (not read)" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --analyze "customers who spent more than 12x"
check_output "§3: a statement separator is refused at the input stage (I07)" "SQL statement syntax: a statement separator" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --embedder deterministic --query "customers from India; select * from orders"
check_output "§3: comment markers are refused, not read around (I08)" "SQL statement syntax: a block-comment opener" \
  env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --embedder deterministic --query "customers/**/from/**/India"
check "§3: a refused request prints no query (exit 1, no SELECT on stdout)" 1 \
  env -C "$V2_DIR" sh -c 'out="$(go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --embedder deterministic --query "customers named Smith; --" 2>/dev/null)"; st=$?; [ -z "$(printf "%s" "$out" | grep -i select)" ] || exit 99; exit $st'
check "§4: resource bounds — input length, maxPhrases (default 256), maxCandidates, nesting depth" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestMaxInputLengthInBothDirections|TestMaxPhrasesComesFromTheConfig|TestDefaultPhraseLimitIsPhase3sLimit|TestCandidateDepthIsCheckedAtStartup|TestCandidatesPerSpanAreRefusedNotTruncated|TestNestingDepthLimit|TestNestingCheckRefusesUnknownPredicates' ./internal/pipeline ./internal/compiler
# §5 and §6 decide between readings, so they need the real model to be shown
# end to end; the rules themselves are covered model-free above.
if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  §5 and §6 sentence checks need the embedding venv"
else
  check_output "§5: a span may not resolve by similarity to a target the sentence names (G34)" "by similarity alone" \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "price of discount"
  check_output "§5: paraphrase with its own configured word is untouched (G35)" '"t0"."total_spent" > $1' \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "customers with amount spent over 100 and total spent below 50"
  check_output "§6: with no configured phrase holding it, the opening verb is the request (G36)" 'SELECT "t0"."price"' \
    env -C "$V2_DIR" go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "list price of products"
fi

echo
echo "== 23. Performance harness and report (Phase 9) =="
check "harness arithmetic, generator and report are tested" 0 \
  env -C "$V2_DIR" go test -count=1 ./internal/bench/ ./cmd/qf-bench/
check_output "scale configs hit their targets exactly (100 / 1k)" "1000 corpus entries" \
  env -C "$V2_DIR" go run ./cmd/qf-bench --suite generate --scales 100,1000
check "the generated core schema is byte-identical to the reviewed config" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestGeneratePreservesTheBaseSchema|TestGenerateIsDeterministic|TestGenerateHitsTargetExactly' ./internal/bench
check "the worker's 2048-text batch limit is chunked, order preserved" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestBatchedSplitsAndPreservesOrder' ./internal/bench
check "percentiles are nearest-rank and under-sampled tails are marked" 0 \
  env -C "$V2_DIR" go test -count=1 -run 'TestPercentileIsNearestRank|TestPercentileRoundsRankUp|TestSummarizeReportsUnderSampledTails' ./internal/bench

# The report is a committed artefact; these check it says what section 17
# requires it to say, not what the numbers are.
REPORT="$V2_DIR/benchmarks/reports/performance.md"
check "a performance report exists" 0 test -f "$REPORT"
for field in "OS / arch" "CPU" "Embedding model" "Model revision" "Index strategy" "Warm latency, sequential" "Concurrency" "CPU and memory"; do
  check_output "report records: $field" "$field" cat "$REPORT"
done
# Stated as one command so it cannot pass vacuously: the table must exist,
# every measured scenario must say "yes", and none may say "no". A grep that
# merely fails to find "no" would also "pass" against a missing report.
check "report shows every scenario served by one worker, none respawned" 0 \
  sh -c 'table=$(grep -A 40 "Warm architecture" "'"$REPORT"'") || exit 1;
         printf "%s" "$table" | grep -q "| yes | " || exit 2;
         printf "%s" "$table" | grep -q -- "| \*\*no\*\* |" && exit 3;
         exit 0'
check_output "report marks a missed budget instead of omitting it" "Target p50" cat "$REPORT"
check_output "report states whether scale changed any answer" "Does scale change the answer" cat "$REPORT"

# The raw results document is scratch output, not a committed artefact, so this
# runs only when a measurement has been taken on this machine. When it has, it
# proves every published percentile, count and throughput figure can be
# recomputed from the per-request samples stored beside it.
if [ -f "$V2_DIR/benchmarks/scratch/results.json" ]; then
  check_output "every reported number recomputes from its raw samples" "0 mismatches" \
    env -C "$V2_DIR" go run ./cmd/qf-bench --suite verify
else
  echo "  SKIP  no measured results on this machine to verify"
fi

if [ ! -x "$V2_DIR/embedding_worker/.venv/bin/python" ]; then
  echo "  SKIP  a live warm measurement needs the embedding venv"
else
  # One short scenario against the real model, into a scratch file: proves the
  # harness measures a warm engine end to end and that the worker is reused.
  check "a live warm scenario runs and reuses one worker" 0 \
    env -C "$V2_DIR" sh -c 'out=$(mktemp -d)/r.json; go run ./cmd/qf-bench --suite warm --scales 100 --reps 1 --warmup 0 --results "$out" >/dev/null 2>&1 || exit 1; grep -q "\"worker_pid_stable\": true" "$out" || exit 2; grep -q "\"embed_per_request\"" "$out" || exit 3'
fi

echo
echo "-- $pass passed, $fail failed --"
[ "$fail" -eq 0 ] || exit 1
echo "Manual gate (phases 0-9): PASS"
