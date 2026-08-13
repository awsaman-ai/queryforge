# Releasing QueryForge

A release happens in **three separate acts**. Only the first is automatic.

| # | Act | Trigger | Reversible? |
|---|-----|---------|-------------|
| 1 | Build, test, tag, GitHub release | automatic on a `v*` tag push | yes — delete the release and the tag |
| 2 | Publish the Python wheels to PyPI | **manual** `workflow_dispatch` | **no** |
| 3 | Publish the Java jars to Maven Central | **manual** `workflow_dispatch` | **no** |

Acts 2 and 3 cannot be selected together. The `publish` input takes one registry per run, so
shipping both languages is two runs. That is deliberate: a failure has one cause, and the
irreversible step gets taken on purpose rather than as a side effect of tagging.

Neither PyPI nor Maven Central lets you reuse a version number. A bad upload burns the number —
you ship `1.1.3`, you do not retag `1.1.2`. The same is true of the Go module proxy once it has
cached a tag.

---

## 1. Tag and release (automatic)

Bump the version everywhere the docs quote it, commit, then:

```bash
git tag -a v1.1.2 -m "v1.1.2 — <what changed>"
git push origin main
git push origin v1.1.2
```

The tag push runs `Release`, which cross-compiles all five platform binaries, runs the Go, Python
and Java suites against the binaries it just built, smoke-tests the Linux wheel in a clean venv,
and attaches the archives plus `checksums.txt` to the GitHub release.

It stops there. Nothing is uploaded to any package registry.

Watch it:

```bash
gh run watch "$(gh run list --workflow=release.yml --limit 1 --json databaseId -q '.[0].databaseId')"
```

Before going further, download the artifacts from that run and check them. This is the last
point at which anything can still be undone.

## 2. Publish to PyPI (manual)

```bash
gh workflow run release.yml -f version=1.1.2 -f publish=pypi
```

Or in the browser: **Actions → Release → Run workflow**, type the version *without* the leading
`v`, set **publish** to `pypi`.

This rebuilds from scratch and re-runs every test, then uploads only the wheels. It uses PyPI
trusted publishing through the `release` environment — no long-lived token — so it needs the
publisher registered against this repo, `release.yml`, and the `release` environment.

The PyPI project is **`queryforge-ai`**, not `queryforge` — that name was taken in May 2026 by an
unrelated project. The import name is unaffected. All four trusted-publisher fields (project
`queryforge-ai`, owner `awsaman-ai`, repo `queryforge`, workflow `release.yml`, environment
`release`) must match exactly or the upload fails with a 403 that does not say which field is wrong.

Verify:

```bash
pip download queryforge-ai==1.1.2 --no-deps -d /tmp/qf-check && ls /tmp/qf-check
```

## 3. Publish to Maven Central (manual)

```bash
gh workflow run release.yml -f version=1.1.2 -f publish=maven
```

Set **publish** to `maven` if using the browser. Needs four secrets on the `release` environment —
`MAVEN_CENTRAL_USERNAME`, `MAVEN_CENTRAL_PASSWORD`, `MAVEN_GPG_PRIVATE_KEY`,
`MAVEN_GPG_PASSPHRASE`. The workflow checks all four are present *before* it starts building, so a
missing one costs a few seconds rather than failing at the signing step with `gpg: no default
secret key`.

Central takes 10–30 minutes to index. The deployment appears in the Central Portal first.

## What to bump before tagging

The workflow rewrites `sdk-python/pyproject.toml`, `sdk-python/queryforge/__init__.py`,
`sdk-java/pom.xml` and `QueryForgeLogging.SDK_VERSION` from the tag at build time, so those are
not the source of truth. The **documentation is** — nothing rewrites it, and a stale version in a
copy-pasteable dependency block is a broken install for whoever copies it.

The Maven and Gradle blocks in `README.md`, `sdk-java/README.md` and `docs/index.html` quote a
`queryforge.version` property set to `LATEST` rather than a literal, so they do not go stale; the
version badge above them is served live by shields.io from Maven Central. Check anything new with:

```bash
grep -rn "<version>" README.md sdk-java/README.md docs/index.html
```

Keep the in-repo defaults in step anyway, so a developer building from a checkout gets a jar and a
wheel that agree with the tree they came from:

```bash
scripts/set-java-sdk-version.sh X.Y.Z                     # the constant every Java log line carries
cd sdk-java && mvn -q versions:set -DnewVersion=X.Y.Z -DgenerateBackupPoms=false
```

plus `version` in `sdk-python/pyproject.toml` and `__version__` in
`sdk-python/queryforge/__init__.py`. The Java pom and the logging constant are held together by
`theSdkVersionMatchesThePom` — bump one without the other and the test fails.
