"""An optional cap on how many engine subprocesses run at once.

Every call into this SDK starts one short-lived engine process and keeps it alive
for as long as the call takes — and a ``translate`` call spends seconds waiting on
a model. A burst of two hundred web requests therefore means two hundred live
processes, which is how a host runs out of memory, file descriptors or PIDs.
Setting :data:`MAX_CONCURRENT_ENV_VAR` makes callers queue for a slot instead.

Scope: the cap is **per Python interpreter process**. Five web-server workers
with a cap of 8 can run 40 engine processes between them, because each worker
holds its own semaphore. That is a property of processes, not something this
module could paper over without inter-process locking.

When the variable is unset — the default — :func:`acquire` returns a shared no-op
slot after one cached check, so existing callers pay nothing measurable and the
request they send is byte-identical to before.
"""

from __future__ import annotations

import os
import re
import threading
import time
from typing import Optional

from .errors import InvalidConfigError, TimeoutError

#: Environment variable holding the cap. Named alongside ``QUERYFORGE_BINARY``
#: and ``QUERYFORGE_LOG_LEVEL``, and identical in the Java SDK.
MAX_CONCURRENT_ENV_VAR = "QUERYFORGE_MAX_CONCURRENT_PROCESSES"

#: Error code raised when no slot frees up before the caller's deadline. It is
#: produced by the SDK, never by the engine, and is the same string in Java.
BUSY_CODE = "SDK_BUSY"

#: Largest accepted cap. Java's semaphore is int-sized; using the same ceiling
#: here keeps a value that works in one SDK from failing in the other.
_MAX_LIMIT = 2**31 - 1

#: Digits only. ``int()`` alone would also accept ``+8``, `` 8_0 `` and full-width
#: digits, none of which Java's parser takes — so both SDKs apply this one rule.
_DIGITS = re.compile(r"^[0-9]+$")


class Slot:
    """A held permit. :meth:`release` must be called exactly once, in ``finally``."""

    __slots__ = ("_semaphore", "wait_ms")

    def __init__(self, semaphore: Optional[threading.BoundedSemaphore], wait_ms: Optional[int]) -> None:
        # None for the no-op slot handed out when no cap is configured.
        self._semaphore = semaphore
        # How long the caller queued, in whole milliseconds; None when uncapped,
        # so the log field is omitted rather than reported as a misleading 0.
        self.wait_ms = wait_ms

    def release(self) -> None:
        """Give the permit back. Idempotent, so a double release cannot inflate the cap."""
        semaphore, self._semaphore = self._semaphore, None
        if semaphore is not None:
            semaphore.release()


#: The slot every call gets when there is no cap. Stateless, so sharing is safe.
_UNLIMITED = Slot(None, None)

# Parsed state, built lazily on the first call and then reused. The lock guards
# only that first build; the hot path reads ``_state`` without locking.
_lock = threading.Lock()
#: None until parsed; afterwards ``(limit, semaphore, error_message)`` where
#: limit/semaphore are None when uncapped and error_message is set when invalid.
_state: Optional[tuple] = None


def _parse(raw: Optional[str]) -> tuple:
    """Turn the variable's raw text into the cached state tuple."""
    value = (raw or "").strip()
    if not value:
        # Unset and set-but-empty mean the same thing, as they do for
        # QUERYFORGE_LOG_LEVEL: `docker run -e VAR=` should not be an error.
        return (None, None, None)
    if not _DIGITS.match(value) or not (1 <= int(value) <= _MAX_LIMIT):
        # Refuse rather than treat as unlimited: someone who set a cap and typed
        # it wrong believes they are protected, and silently being unprotected is
        # the one outcome worse than a loud error.
        return (
            None,
            None,
            f"{MAX_CONCURRENT_ENV_VAR} must be a whole number between 1 and {_MAX_LIMIT}, "
            f"got {value!r}. Unset it to run without a limit.",
        )
    limit = int(value)
    # Bounded, so a release without a matching acquire raises instead of quietly
    # raising the cap by one.
    return (limit, threading.BoundedSemaphore(limit), None)


def _load() -> tuple:
    """Return the parsed state, parsing the environment on first use only."""
    global _state
    state = _state
    if state is None:
        with _lock:
            if _state is None:
                _state = _parse(os.environ.get(MAX_CONCURRENT_ENV_VAR))
            state = _state
    return state


def acquire(timeout_seconds: Optional[float]) -> Slot:
    """Wait for a slot, honouring the caller's deadline.

    With no cap this returns immediately. With a cap it blocks — on a condition
    variable, not a poll — until a slot frees up or ``timeout_seconds`` passes,
    in which case it raises :class:`~queryforge.errors.TimeoutError` with code
    ``SDK_BUSY``. ``None`` means the caller set no deadline, so it waits.
    """
    _, semaphore, error = _load()
    if error:
        # Raised on every call rather than once: a config error that fires only
        # for the first request would be lost in whichever log that request hit.
        raise InvalidConfigError(error, code="INVALID_CONFIG")
    if semaphore is None:
        return _UNLIMITED

    started = time.monotonic()
    if not semaphore.acquire(timeout=timeout_seconds):
        # Only reachable with a deadline: acquire(timeout=None) cannot return False.
        raise busy_error(timeout_seconds)
    return Slot(semaphore, int((time.monotonic() - started) * 1000))


def busy_error(timeout_seconds: Optional[float]) -> TimeoutError:
    """Build the ``SDK_BUSY`` error for a caller whose deadline ran out in the queue."""
    cap = (_state or (None,))[0]
    return TimeoutError(
        f"No QueryForge engine slot became free within {timeout_seconds:.3g}s: all "
        f"{cap} slots allowed by {MAX_CONCURRENT_ENV_VAR} stayed busy. Raise the limit, "
        f"raise the timeout, or reduce concurrent calls.",
        code=BUSY_CODE,
    )


def _reset() -> None:
    """Forget the parsed state so the next call re-reads the environment.

    Used by the test suite, and after ``fork``: a child process inherits the
    parent's semaphore *with the parent's in-flight permits already taken*, but
    not the threads that would release them, so without a reset a forked worker
    could start life with fewer slots than configured — or none. The lock is
    replaced too, since a lock held by another thread at fork time stays held
    forever in the child.
    """
    global _state, _lock
    _lock = threading.Lock()
    _state = None


# POSIX only; Windows has no fork and no os.register_at_fork.
if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_reset)
