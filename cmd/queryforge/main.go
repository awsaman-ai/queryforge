// Command queryforge is the executable the language SDKs drive. It reads one
// JSON request from stdin, runs it against the QueryForge engine, and writes one
// JSON response to stdout.
//
// It is deliberately not a server. There is no HTTP, no port, no daemon and no
// listening socket: an SDK spawns this process, writes a request, reads the
// reply, and the process exits. That keeps the security surface at "a local
// subprocess with the caller's own privileges" and means a Java or Python
// program can depend on QueryForge without operating anything.
//
// Everything that is not the response — warnings, diagnostics, panics — goes to
// stderr. stdout carries exactly one JSON object and nothing else, because that
// is the entire contract the SDKs parse against.
//
// Usage:
//
//	echo '{"op":"version"}' | queryforge
//	queryforge < request.json > response.json
//	queryforge --request request.json      # read from a file instead of stdin
//
// Exit codes:
//
//	0  the response has success:true
//	1  the response has success:false (a well-formed error object is on stdout)
//	2  no response could be produced at all (stdout is empty; see stderr)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Version is the binary's own version, stamped at build time with
// -ldflags "-X main.Version=1.2.3". It defaults to "dev" for a plain `go build`
// so a locally built binary is never mistaken for a release.
var Version = "dev"

// maxRequestBytes caps how much stdin will be read. A request carries a config
// and possibly an AST, both of which are small; anything past this is a runaway
// producer or a mistake, and reading it would just trade a clear error for an
// out-of-memory kill. 32 MB is far above any real config and far below trouble.
const maxRequestBytes = 32 << 20

// exit codes; see the package comment.
const (
	exitOK       = 0
	exitFailure  = 1
	exitProtocol = 2
)

func main() {
	requestPath := flag.String("request", "", "read the JSON request from this file instead of stdin")
	showVersion := flag.Bool("version", false, "print the binary version and exit")
	pretty := flag.Bool("pretty", false, "indent the JSON response (for humans; SDKs do not set this)")
	flag.Parse()

	// --version is the one human affordance. It prints a bare version string
	// rather than a JSON object, because it exists for `queryforge --version` at
	// a shell prompt; an SDK handshake uses the `version` op and gets JSON.
	if *showVersion {
		fmt.Println(Version)
		os.Exit(exitOK)
	}

	os.Exit(run(*requestPath, *pretty, os.Stdin, os.Stdout, os.Stderr))
}

// run is main's body with its dependencies passed in, so the whole process
// contract — parse, dispatch, encode, choose an exit code — is testable without
// spawning anything.
func run(requestPath string, pretty bool, stdin io.Reader, stdout, stderr io.Writer) int {
	src := stdin
	if requestPath != "" {
		f, err := os.Open(requestPath)
		if err != nil {
			fmt.Fprintf(stderr, "queryforge: cannot read request file: %v\n", err) //nolint:errcheck // a diagnostic; if stderr is gone there is nowhere left to report that
			return exitProtocol
		}
		defer f.Close() //nolint:errcheck // read-only file; a close error cannot affect the result
		src = f
	}

	data, err := io.ReadAll(io.LimitReader(src, maxRequestBytes+1))
	if err != nil {
		fmt.Fprintf(stderr, "queryforge: cannot read request: %v\n", err) //nolint:errcheck // a diagnostic; if stderr is gone there is nowhere left to report that
		return exitProtocol
	}
	if len(data) > maxRequestBytes {
		return write(stdout, stderr, pretty, errorResponse("", CodeInvalidRequest,
			fmt.Sprintf("request exceeds the %d byte limit", maxRequestBytes)))
	}

	req, resp := decodeRequest(data)
	if resp != nil {
		return write(stdout, stderr, pretty, resp)
	}

	// The deadline covers the whole operation, model call included, and is the
	// only thing standing between a hung endpoint and an SDK caller blocked
	// forever on a subprocess read.
	timeout := defaultTimeout
	if req.Options.TimeoutMS > 0 {
		timeout = time.Duration(req.Options.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return write(stdout, stderr, pretty, handle(ctx, req))
}

// decodeRequest parses the request body, returning either a request or the error
// response to send. Splitting it out keeps the three distinct JSON failures —
// empty input, malformed JSON, a field of the wrong type — each getting the
// message that actually points at the mistake, instead of one generic
// "invalid request".
func decodeRequest(data []byte) (*Request, *Response) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errorResponse("", CodeInvalidRequest,
			"empty request: expected one JSON object on stdin, e.g. {\"op\":\"version\"}")
	}

	var req Request
	dec := json.NewDecoder(bytes.NewReader(data))
	// Unknown fields are rejected rather than ignored. A misspelled "scop" or
	// "backends" would otherwise be silently dropped and the caller would get a
	// correct-looking query built from the wrong inputs — the single worst
	// failure mode this protocol could have, because nothing in the output looks
	// wrong.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, errorResponse("", CodeInvalidRequest, fmt.Sprintf(
				"field %q must be a %s, got %s", typeErr.Field, typeErr.Type, typeErr.Value))
		}
		return nil, errorResponse("", CodeInvalidRequest, "request is not valid JSON: "+err.Error())
	}

	// Exactly one object, nothing after it. A second object would be silently
	// ignored, which for a caller batching requests is a lost query with no
	// error — so say so instead.
	if dec.More() {
		return nil, errorResponse(req.Op, CodeInvalidRequest,
			"request carries trailing content after the JSON object; send exactly one object per invocation")
	}
	return &req, nil
}

// write encodes the response to stdout and returns the process exit code.
//
// An encoding failure here is the one case where the contract cannot be met, so
// it degrades deliberately: emit a hand-built JSON object that is guaranteed to
// serialize, so even this path leaves the SDK with something parseable rather
// than a closed pipe.
func write(stdout, stderr io.Writer, pretty bool, resp *Response) int {
	enc := json.NewEncoder(stdout)
	if pretty {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(resp); err != nil {
		// Both writes below are unchecked deliberately: this is already the failure path, and if
		// the pipes are gone too there is nothing left to report the failure with.
		fmt.Fprintf(stderr, "queryforge: cannot encode response: %v\n", err) //nolint:errcheck // see above

		fmt.Fprintf(stdout, `{"success":false,"protocol":%q,"code":%q,"message":"response could not be encoded"}`+"\n", //nolint:errcheck // see above
			ProtocolVersion, CodeInternal)
		return exitProtocol
	}
	if resp.Success {
		return exitOK
	}
	return exitFailure
}
