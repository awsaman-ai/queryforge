module github.com/awsaman-ai/queryforge

// The oldest Go this library supports, matching the README badge and the CI
// matrix ("current release plus the previous one"). It is deliberately a MINOR
// version, not a patch: a `go 1.26.4` directive in a library is imposed on every
// downstream consumer, so a build on Go 1.25 with GOTOOLCHAIN=local — common in
// locked-down CI and in distro packaging — failed outright with "go.mod requires
// go >= 1.26.4", and with the default GOTOOLCHAIN=auto silently downloaded a
// second toolchain. Newer requirements belong in a `toolchain` line, which is
// advisory for consumers.
go 1.25
