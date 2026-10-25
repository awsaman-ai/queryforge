#!/usr/bin/env bash
#
# Builds the Maven Central bundle exactly as the release does, without uploading it, and checks
# its layout the way Central's validator does.
#
# Why this exists: Central validates a bundle only after it is uploaded, so a layout Central
# rejects ("Bundle has content that does NOT have a .pom file: io/github/awsaman-ai/queryforge",
# v1.2.7) used to surface only in the manual publish job, after the tag and the PyPI release.
# The plugin's skipUpload mode builds the same zip with no credentials, so CI can fail first.
#
# Rule checked: every file in the bundle must sit in <group path>/<artifactId>/<version>/ and be
# named <artifactId>-<version>*, and every such directory must contain <artifactId>-<version>.pom.
# Anything else, such as a maven-metadata file one level up, is what Central rejects.
#
# The plugin version comes from sdk-java/pom.xml, so this checks what the release will use.
#
# Usage (from anywhere):
#   scripts/check-central-bundle.sh

set -euo pipefail

cd "$(dirname "$0")/../sdk-java"
rm -rf target/central-publishing target/central-staging

# -Dgpg.skip: signing needs the release key, and the layout is the same without .asc files.
mvn -B -q -Prelease -DskipTests -Dgpg.skip=true -Dmaven.javadoc.skip=true -DskipUpload=true \
  package org.sonatype.central:central-publishing-maven-plugin:publish

zip=target/central-publishing/central-bundle.zip
[ -f "$zip" ] || { echo "::error::no bundle was produced at $zip" >&2; exit 1; }

unzip -Z1 "$zip" | awk '
  /\/$/ { next }                                   # directory entries carry no content
  {
    n = split($0, p, "/")
    file = p[n]; ver = p[n-1]; art = p[n-2]
    dir = $0; sub(/\/[^\/]*$/, "", dir)
    if (n < 4 || index(file, art "-" ver) != 1) { bad[$0] = 1; next }
    if (file == art "-" ver ".pom") pom[dir] = 1
    dirs[dir] = 1
  }
  END {
    for (d in dirs) if (!(d in pom)) { print "no .pom in " d; fail = 1 }
    for (f in bad) { print "misplaced file: " f; fail = 1 }
    if (!length(dirs)) { print "bundle is empty"; fail = 1 }
    exit fail
  }' || { echo "::error::Central bundle layout is invalid (see above)" >&2; exit 1; }

echo "Central bundle layout OK: $(unzip -Z1 "$zip" | grep -vc '/$') files"
