// The v2 experiment is a SEPARATE module from github.com/awsaman-ai/queryforge.
//
// The path is deliberately not "github.com/awsaman-ai/queryforge/v2": in Go a
// final path element of "v2" is a MAJOR VERSION suffix, so that name would claim
// this is version 2 of the production library and would demand v2.x.x tags. It is
// not — it is an isolated research track that may well be deleted.
//
// ".local" is not a real TLD, so the path is unresolvable by design: `go get` can
// never fetch it and it can never be published by accident.
module queryforge.local/v2experiment

// Matches the parent library's floor rather than the installed toolchain (1.26.4)
// so the experiment keeps building on the same Go versions CI already covers.
go 1.25

// No requires. The experiment has zero third-party dependencies at Phase 0, and
// internal/isolation/isolation_test.go fails the build if that ever stops being
// true without a decision record. See docs/decisions/002-module-identity.md.
