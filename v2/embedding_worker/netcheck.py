"""Network check for the embedding worker (brief section 16.11; Phase 8).

Runs worker.py in THIS interpreter under a Python audit hook that records
every socket connection and every DNS lookup the process makes, feeds it the
protocol requests on stdin as usual, and reports on stderr:

    NETWORK_EVENTS=<n>
      <event> <arguments>      (one line per event, at most 20)

The worker's own stdout (the JSON protocol) passes through untouched.

Usage:

    .venv/bin/python netcheck.py worker.py < requests.jsonl
    .venv/bin/python netcheck.py --self-test

--self-test proves the hook can see a connection at all, without the
internet: it connects to a listening loopback socket and must count at least
one event. A check that always reports zero would prove nothing (the Phase 4
lesson: a sweep that could not fail proved nothing), so the Go test runs the
self-test before trusting a zero.

Audit hooks see what the process does, including inside C extensions that go
through Python's socket module — which is how huggingface_hub, requests and
urllib3 reach the network. It does not see a native library that opens its
own sockets; none in the worker's stack does.
"""

import runpy
import socket
import sys

EVENTS = ("socket.connect", "socket.getaddrinfo", "socket.gethostbyname")
seen = []


def hook(event, args):
    if event in EVENTS:
        seen.append((event, repr(args)[:160]))


def report():
    sys.stderr.write("NETWORK_EVENTS=%d\n" % len(seen))
    for event, args in seen[:20]:
        sys.stderr.write("  %s %s\n" % (event, args))
    sys.stderr.flush()


def self_test() -> int:
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.bind(("127.0.0.1", 0))
    listener.listen(1)
    client = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    client.connect(listener.getsockname())
    client.close()
    listener.close()
    return 0


def main() -> int:
    sys.addaudithook(hook)
    if sys.argv[1:] == ["--self-test"]:
        try:
            return self_test()
        finally:
            report()
    if len(sys.argv) != 2:
        sys.stderr.write("usage: netcheck.py <worker.py> | --self-test\n")
        return 2
    script = sys.argv[1]
    sys.argv = [script]
    code = 0
    try:
        runpy.run_path(script, run_name="__main__")
    except SystemExit as exc:
        code = exc.code if isinstance(exc.code, int) else 0
    finally:
        report()
    return code


if __name__ == "__main__":
    sys.exit(main())
