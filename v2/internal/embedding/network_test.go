package embedding

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Section 16.11 (Phase 8, decision 021): no natural-language query leaves the
// machine during normal execution. Phase 8 measured that the worker contacted
// huggingface.co on every start, although the weights were cached; it now
// runs offline unless QF_V2_ALLOW_MODEL_DOWNLOAD=1.
//
// The check runs the real worker under a Python audit hook
// (embedding_worker/netcheck.py) through a full identity + embed + shutdown
// exchange, and requires ZERO socket connections and DNS lookups. It first
// runs the hook's self-test, which must see a loopback connection: a check
// that cannot see a connection cannot vouch for the absence of one.
// Skipped, like every worker test, when the virtualenv does not exist.

var networkEvents = regexp.MustCompile(`NETWORK_EVENTS=(\d+)`)

func netcheck(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	if ok, why := WorkerAvailable(""); !ok {
		t.Skipf("embedding worker unavailable: %s", why)
	}
	script, err := resolveScript("")
	if err != nil {
		t.Fatal(err)
	}
	python, err := resolvePython("", script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, append([]string{filepath.Join(filepath.Dir(script), "netcheck.py")}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		t.Fatalf("netcheck %v: %v\n%s", args, err, stderr.String())
	}
	m := networkEvents.FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("netcheck printed no count:\n%s", stderr.String())
	}
	n, _ := strconv.Atoi(m[1])
	return n, stderr.String()
}

func TestNetworkCheckCanSeeAConnection(t *testing.T) {
	if n, out := netcheck(t, "", "--self-test"); n < 1 {
		t.Fatalf("the audit hook missed a loopback connection, so its zero means nothing:\n%s", out)
	}
}

func TestWorkerMakesNoNetworkConnections(t *testing.T) {
	TestNetworkCheckCanSeeAConnection(t)
	// The opt-in must not leak in from the environment the tests run in.
	t.Setenv("QF_V2_ALLOW_MODEL_DOWNLOAD", "")
	t.Setenv("HF_HUB_OFFLINE", "0") // a stray setting must not reopen the network

	script, _ := resolveScript("")
	requests := `{"op":"identity"}` + "\n" +
		`{"op":"embed","texts":["customers from India who spent more than 50000"]}` + "\n" +
		`{"op":"shutdown"}` + "\n"
	n, out := netcheck(t, requests, script)
	if n != 0 {
		t.Fatalf("the worker made %d network events in normal execution:\n%s", n, out)
	}
	// And it still knows exactly which weights it runs: offline, the
	// revision comes from the local cache, not from the hub.
	if strings.Contains(out, "revision=unknown") {
		t.Errorf("offline, the worker lost its revision:\n%s", out)
	}
}
