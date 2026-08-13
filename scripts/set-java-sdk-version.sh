#!/usr/bin/env bash
#
# Rewrites the version constant the Java SDK reports on every log record.
#
# The pom is not the only place the Java SDK states its version. QueryForgeLogging.SDK_VERSION is
# compiled into the jar and stamped onto every log line, because a log aggregator has to be able
# to answer "which build produced this?" without the record naming a file. `mvn versions:set`
# rewrites the pom and nothing else, so the release has to rewrite the constant too.
#
# The two are held together by theSdkVersionMatchesThePom, which reads the pom at test time. That
# guard is what makes this script mandatory rather than cosmetic: bumping the pom alone turns the
# release build red at the Java step, after the binaries have already been cross-compiled.
#
# Usage:
#   scripts/set-java-sdk-version.sh 1.1.3

set -euo pipefail

VERSION="${1:?usage: set-java-sdk-version.sh <version>}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TARGET="$REPO_ROOT/sdk-java/src/main/java/io/queryforge/QueryForgeLogging.java"

# Refuse anything that is not a bare semver. The constant ends up in every log line and in the
# jar's own report of itself, so a "v1.1.3" here would be visible for the life of the release.
if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$'; then
  echo "error: '$VERSION' is not a bare semver such as 1.1.3" >&2
  exit 1
fi

# Written through a temp file rather than `sed -i`, whose in-place flag takes an argument on BSD
# sed and not on GNU sed — the release runs on Linux, but this is also run by hand on a Mac.
TMP="$(mktemp)"
sed -E "s/(static final String SDK_VERSION = )\"[^\"]*\";/\1\"$VERSION\";/" "$TARGET" >"$TMP"

# A sed that matched nothing exits 0, which would leave the old version in place and hand the
# release exactly the silent drift this script exists to prevent. Verify the value landed.
if ! grep -q "static final String SDK_VERSION = \"$VERSION\";" "$TMP"; then
  rm -f "$TMP"
  echo "error: SDK_VERSION declaration not found in $TARGET" >&2
  exit 1
fi

mv "$TMP" "$TARGET"
echo "SDK_VERSION set to $VERSION"
