"""QUERYFORGE_MAX_CONCURRENT_PROCESSES: the optional cap on live engine processes.

Most tests here run against a *probe* fake binary rather than the shared
``fake_binary`` fixture. Each probe process creates a marker directory named for
its PID on start, counts the markers present, sleeps, and removes its own marker
on exit. A marker exists only while its process is alive, so the largest count
any probe observed can never exceed the true number of simultaneous processes.
That makes "the cap held" a measurement rather than an inference from timing.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
from pathlib import Path

import pytest

from queryforge import (
    InvalidConfigError,
    ProtocolError,
    QueryForge,
    QueryForgeError,
    TimeoutError,
    _limit,
)

ENV = "QUERYFORGE_MAX_CONCURRENT_PROCESSES"


@pytest.fixture
def forge(config):
    return QueryForge.postgres(config)


class Probe:
    """Handle on an installed probe binary: what each process saw and was sent."""

    def __init__(self, slots: Path, log: Path) -> None:
        self.slots = slots
        self.log = log

    def runs(self) -> list[dict]:
        if not self.log.is_file():
            return []
        return [json.loads(line) for line in self.log.read_text().splitlines() if line.strip()]

    def peak(self) -> int:
        return max((run["seen"] for run in self.runs()), default=0)

    def wait_until_running(self, count: int = 1, within: float = 5.0) -> None:
        """Block until `count` probes are alive — test-side polling, not SDK polling."""
        deadline = time.monotonic() + within
        while time.monotonic() < deadline:
            if self.slots.is_dir() and len(os.listdir(self.slots)) >= count:
                return
            time.sleep(0.01)
        raise AssertionError(f"{count} probe process(es) never started")


@pytest.fixture
def probe(tmp_path: Path, monkeypatch: pytest.MonkeyPatch, ok_response):
    """Install a probe engine that sleeps `sleep_seconds` and then answers."""

    def make(sleep_seconds: float = 0.4, response: dict | str | None = None) -> Probe:
        slots = tmp_path / "slots"
        slots.mkdir(exist_ok=True)
        log = tmp_path / "probe.jsonl"
        body = response if isinstance(response, str) else json.dumps(response or ok_response(sql="SELECT 1"))
        script = tmp_path / "probe.py"
        script.write_text(
            "import json, os, sys, time\n"
            f"slots = {str(slots)!r}\n"
            "me = os.path.join(slots, str(os.getpid()))\n"
            "os.mkdir(me)\n"
            "seen = len(os.listdir(slots))\n"
            "raw = sys.stdin.read()\n"
            f"time.sleep({sleep_seconds!r})\n"
            f"open({str(log)!r}, 'a').write(json.dumps({{'seen': seen, 'request': json.loads(raw)}}) + '\\n')\n"
            "os.rmdir(me)\n"
            f"sys.stdout.write({body!r})\n"
        )
        launcher = tmp_path / "probe"
        launcher.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{script}" "$@"\n')
        launcher.chmod(0o755)
        monkeypatch.setenv("QUERYFORGE_BINARY", str(launcher))
        return Probe(slots, log)

    return make


def timeout_sent(run: dict):
    """The ``options.timeoutMs`` a probe run was sent, or None."""
    return (run["request"].get("options") or {}).get("timeoutMs")


def run_parallel(fn, callers: int, within: float = 30.0) -> list:
    """Run `fn` from `callers` threads at once; return results or raised exceptions.

    Daemon threads joined with a limit, not a pool: if a slot ever leaks, a
    caller with no deadline waits forever, and that has to surface as a failing
    assertion here rather than as a suite that hangs.
    """
    start = threading.Barrier(callers)
    results: list = [None] * callers

    def call(i):
        start.wait()
        try:
            results[i] = fn()
        except BaseException as exc:  # noqa: BLE001 - collected for the assertion
            results[i] = exc

    threads = [threading.Thread(target=call, args=(i,), daemon=True) for i in range(callers)]
    for t in threads:
        t.start()
    deadline = time.monotonic() + within
    for t in threads:
        t.join(max(0.0, deadline - time.monotonic()))
    stuck = sum(t.is_alive() for t in threads)
    assert not stuck, f"{stuck} caller(s) never finished — a slot was not released"
    return results


def in_background(fn) -> threading.Thread:
    """Start one call on a daemon thread (see run_parallel for why daemon)."""
    thread = threading.Thread(target=fn, daemon=True)
    thread.start()
    return thread


def finish(thread: threading.Thread, within: float = 30.0) -> None:
    thread.join(within)
    assert not thread.is_alive(), "background call never finished — a slot was not released"


pytestmark = pytest.mark.skipif(sys.platform.startswith("win"), reason="probe binary is a POSIX shell shim")


# --- default: no cap -----------------------------------------------------------


def test_unset_imposes_no_limit(probe, forge):
    p = probe(sleep_seconds=0.6)
    results = run_parallel(lambda: forge.query("orders").to_sql(), 6)
    assert all(not isinstance(r, BaseException) for r in results), results
    # Proves the probe can see concurrency at all, so the capped tests below
    # are measuring the cap and not a probe that always reports 1.
    assert p.peak() >= 3


def test_unset_sends_the_request_unchanged(probe, forge, monkeypatch):
    p = probe(sleep_seconds=0)
    forge.query("orders").timeout(2.5).to_sql()
    monkeypatch.setenv(ENV, "1")
    _limit._reset()
    forge.query("orders").timeout(2.5).to_sql()
    uncapped, capped = (run["request"] for run in p.runs())
    # An uncontended capped call waited 0 ms, so nothing is deducted.
    assert uncapped == capped
    assert capped["options"]["timeoutMs"] == 2500


# --- the cap holds -------------------------------------------------------------


@pytest.mark.parametrize("cap", [1, 2, 3])
def test_cap_bounds_simultaneous_processes(probe, forge, monkeypatch, cap):
    monkeypatch.setenv(ENV, str(cap))
    p = probe(sleep_seconds=0.4)
    results = run_parallel(lambda: forge.query("orders").to_sql(), 7)
    assert all(not isinstance(r, BaseException) for r in results), results
    assert len(p.runs()) == 7
    assert p.peak() == cap


def test_waiting_callers_proceed_when_a_slot_frees(probe, forge, monkeypatch):
    monkeypatch.setenv(ENV, "1")
    p = probe(sleep_seconds=0.3)
    started = time.monotonic()
    results = run_parallel(lambda: forge.query("orders").to_sql(), 3)
    elapsed = time.monotonic() - started
    assert results == ["SELECT 1"] * 3
    assert p.peak() == 1
    # Serialised: three sleeps back to back, not overlapping.
    assert elapsed >= 0.85


def test_large_responses_do_not_deadlock_under_a_cap(probe, forge, monkeypatch, ok_response):
    monkeypatch.setenv(ENV, "1")
    big = "SELECT " + "x" * (4 * 1024 * 1024)
    probe(sleep_seconds=0, response=ok_response(sql=big))
    results = run_parallel(lambda: forge.query("orders").timeout(20).to_sql(), 3)
    assert [len(r) for r in results] == [len(big)] * 3


# --- deadlines -----------------------------------------------------------------


def test_deadline_expiring_in_the_queue_raises_sdk_busy(probe, forge, monkeypatch):
    monkeypatch.setenv(ENV, "1")
    p = probe(sleep_seconds=1.5)
    holder = in_background(lambda: forge.query("orders").to_sql())
    p.wait_until_running()

    started = time.monotonic()
    with pytest.raises(TimeoutError) as exc:
        forge.query("orders").timeout(0.2).to_sql()
    waited = time.monotonic() - started
    finish(holder)

    assert exc.value.code == "SDK_BUSY"
    assert isinstance(exc.value, QueryForgeError)
    assert ENV in str(exc.value)
    assert 0.15 <= waited < 1.0
    # The timed-out caller never started a process.
    assert len(p.runs()) == 1


def test_queue_time_comes_out_of_the_engine_deadline(probe, forge, monkeypatch):
    monkeypatch.setenv(ENV, "1")
    p = probe(sleep_seconds=0.8)
    holder = in_background(lambda: forge.query("orders").to_sql())
    p.wait_until_running()
    forge.query("orders").timeout(5).to_sql()
    finish(holder)

    sent = [t for t in (timeout_sent(run) for run in p.runs()) if t is not None]
    assert len(sent) == 1
    # Roughly 0.8 s of the 5 s budget was spent queued.
    assert 3500 <= sent[0] <= 4900


def test_no_deadline_waits_for_as_long_as_it_takes(probe, forge, monkeypatch):
    monkeypatch.setenv(ENV, "1")
    p = probe(sleep_seconds=0.6)
    results = run_parallel(lambda: forge.query("orders").to_sql(), 2)
    assert results == ["SELECT 1"] * 2
    assert all(timeout_sent(run) is None for run in p.runs()), "no deadline was set, so none may be invented"


def test_slot_arriving_with_no_budget_left_is_busy_and_released(fake_binary, forge, monkeypatch, ok_response):
    monkeypatch.setenv(ENV, "1")
    binary = fake_binary(ok_response(sql="SELECT 1"))
    real_acquire = _limit.acquire

    def slow_acquire(timeout_seconds):
        slot = real_acquire(timeout_seconds)
        # Pretend the whole budget went on queueing.
        time.sleep(timeout_seconds + 0.05)
        slot.wait_ms = int((timeout_seconds + 0.05) * 1000)
        return slot

    monkeypatch.setattr(_limit, "acquire", slow_acquire)
    with pytest.raises(TimeoutError) as exc:
        forge.query("orders").timeout(0.1).to_sql()
    assert exc.value.code == "SDK_BUSY"
    assert binary.call_count == 0
    assert _limit._state[1]._value == 1


# --- permits always come back ----------------------------------------------------


def _fails_with_error_response(fake_binary, ok_response, err_response, monkeypatch):
    fake_binary(err_response("VALIDATION_FAILED"))
    return QueryForgeError


def _fails_with_crash(fake_binary, ok_response, err_response, monkeypatch):
    fake_binary(None, exit_code=2, stderr="panic: boom")
    return ProtocolError


def _fails_with_garbage(fake_binary, ok_response, err_response, monkeypatch):
    fake_binary("not json at all")
    return ProtocolError


def _fails_with_protocol_mismatch(fake_binary, ok_response, err_response, monkeypatch):
    fake_binary(ok_response(protocol="9.0"))
    return ProtocolError


def _fails_with_kill(fake_binary, ok_response, err_response, monkeypatch):
    from queryforge import _transport

    monkeypatch.setattr(_transport, "_KILL_GRACE_SECONDS", 0.2)
    fake_binary(ok_response(), sleep_seconds=10)
    return ProtocolError


def _fails_to_start(fake_binary, ok_response, err_response, monkeypatch):
    from queryforge import _transport

    fake_binary(ok_response())

    def boom(*args, **kwargs):
        raise OSError("exec format error")

    monkeypatch.setattr(_transport.subprocess, "run", boom)
    return ProtocolError


def _interrupted(fake_binary, ok_response, err_response, monkeypatch):
    from queryforge import _transport

    fake_binary(ok_response())

    def interrupt(*args, **kwargs):
        raise KeyboardInterrupt

    monkeypatch.setattr(_transport.subprocess, "run", interrupt)
    return KeyboardInterrupt


@pytest.mark.parametrize(
    "arrange",
    [
        _fails_with_error_response,
        _fails_with_crash,
        _fails_with_garbage,
        _fails_with_protocol_mismatch,
        _fails_with_kill,
        _fails_to_start,
        _interrupted,
    ],
)
def test_slot_is_released_on_every_failure(arrange, fake_binary, forge, monkeypatch, ok_response, err_response):
    monkeypatch.setenv(ENV, "1")
    expected = arrange(fake_binary, ok_response, err_response, monkeypatch)
    with pytest.raises(expected):
        forge.query("orders").timeout(0.1).to_sql()
    assert _limit._state[1]._value == 1

    # And behaviourally: with the one slot leaked, this would be SDK_BUSY.
    monkeypatch.undo()
    monkeypatch.setenv(ENV, "1")
    fake_binary(ok_response(sql="SELECT 2"))
    assert forge.query("orders").timeout(2).to_sql() == "SELECT 2"


def test_double_release_cannot_raise_the_cap(monkeypatch):
    monkeypatch.setenv(ENV, "1")
    slot = _limit.acquire(None)
    slot.release()
    slot.release()
    assert _limit._state[1]._value == 1


# --- configuration ---------------------------------------------------------------


@pytest.mark.parametrize(
    "bad",
    ["0", "-1", "abc", "1.5", "8_0", "+3", "2147483648", "\uff18", "4 slots", "0x10"],
)
def test_invalid_values_fail_clearly_and_start_nothing(fake_binary, forge, monkeypatch, ok_response, bad):
    monkeypatch.setenv(ENV, bad)
    binary = fake_binary(ok_response())
    for _ in range(2):  # every call, not only the first
        with pytest.raises(InvalidConfigError) as exc:
            forge.query("orders").to_sql()
        assert exc.value.code == "INVALID_CONFIG"
        assert ENV in str(exc.value)
    assert binary.call_count == 0


@pytest.mark.parametrize("blank", ["", "   "])
def test_blank_value_means_no_limit(fake_binary, forge, monkeypatch, ok_response, captured_logs, blank):
    monkeypatch.setenv(ENV, blank)
    fake_binary(ok_response(sql="SELECT 1"))
    assert forge.query("orders").to_sql() == "SELECT 1"
    assert all("wait_ms" not in f for f in captured_logs.fields())


def test_surrounding_whitespace_and_leading_zeros_are_accepted(fake_binary, forge, monkeypatch, ok_response):
    monkeypatch.setenv(ENV, " 04 ")
    fake_binary(ok_response(sql="SELECT 1"))
    forge.query("orders").to_sql()
    assert _limit._state[0] == 4


def test_value_is_read_once(fake_binary, forge, monkeypatch, ok_response):
    monkeypatch.setenv(ENV, "2")
    fake_binary(ok_response(sql="SELECT 1"))
    forge.query("orders").to_sql()
    monkeypatch.setenv(ENV, "not a number")
    # Still the cap parsed on first use; changing the variable mid-process does nothing.
    assert forge.query("orders").to_sql() == "SELECT 1"
    assert _limit._state[0] == 2


@pytest.mark.skipif(not hasattr(os, "fork"), reason="needs fork")
def test_forked_child_starts_with_a_full_set_of_slots(monkeypatch):
    monkeypatch.setenv(ENV, "1")
    held = _limit.acquire(None)  # the parent's in-flight call
    try:
        pid = os.fork()
        if pid == 0:  # pragma: no cover - runs in the child
            try:
                slot = _limit.acquire(0.2)
                slot.release()
                os._exit(0)
            except BaseException:
                os._exit(1)
        _, status = os.waitpid(pid, 0)
    finally:
        held.release()
    assert os.WEXITSTATUS(status) == 0


# --- logging ------------------------------------------------------------------------


def test_capped_calls_log_their_queue_time(probe, forge, monkeypatch, captured_logs):
    monkeypatch.setenv(ENV, "1")
    probe(sleep_seconds=0.3)
    run_parallel(lambda: forge.query("orders").scope({"tenantId": "t-SECRET-42"}).to_sql(), 2)
    done = [f for f in captured_logs.fields() if f.get("outcome") == "ok"]
    assert len(done) == 2
    waits = sorted(f["wait_ms"] for f in done)
    assert waits[0] < 200 and waits[1] >= 200
    # duration_ms covers the whole call, queue included.
    assert all(f["duration_ms"] >= f["wait_ms"] for f in done)
    assert "t-SECRET-42" not in captured_logs.text()


def test_uncapped_calls_have_no_wait_field(fake_binary, forge, ok_response, captured_logs):
    fake_binary(ok_response(sql="SELECT 1"))
    forge.query("orders").to_sql()
    assert captured_logs.fields()
    assert all("wait_ms" not in f for f in captured_logs.fields())


def test_busy_is_logged_once_at_error_with_its_code(probe, forge, monkeypatch, captured_logs):
    import logging

    monkeypatch.setenv(ENV, "1")
    p = probe(sleep_seconds=1.0)
    holder = in_background(lambda: forge.query("orders").to_sql())
    p.wait_until_running()
    with pytest.raises(TimeoutError):
        forge.query("orders").timeout(0.1).to_sql()
    finish(holder)
    errors = [r for r in captured_logs.records if r.levelno >= logging.ERROR]
    assert len(errors) == 1
    assert errors[0].queryforge["error_code"] == "SDK_BUSY"


# --- against the real engine ----------------------------------------------------------


def test_real_engine_results_are_identical_under_a_cap(real_binary, config, valid_ast, monkeypatch):
    forge = QueryForge.postgres(config)
    expected = forge.generate(valid_ast)

    monkeypatch.setenv(ENV, "2")
    _limit._reset()
    results = run_parallel(lambda: forge.generate(valid_ast), 8)
    assert all(not isinstance(r, BaseException) for r in results), results
    assert {(r.sql, r.args) for r in results} == {(expected.sql, expected.args)}
