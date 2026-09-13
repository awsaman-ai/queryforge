package io.queryforge;

import java.util.Collections;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * An optional cap on how many engine subprocesses run at once.
 *
 * <p>Every call into this SDK starts one short-lived engine process and keeps it alive for as long
 * as the call takes — and a {@code translate} call spends seconds waiting on a model. A burst of
 * two hundred request threads therefore means two hundred live processes, which is how a host runs
 * out of memory, file descriptors or PIDs. Setting {@value #MAX_CONCURRENT_ENV_VAR} (or the
 * {@code -D}{@value #MAX_CONCURRENT_PROPERTY} system property, which wins) makes callers queue for
 * a slot instead.
 *
 * <p>Scope: the cap is <strong>JVM-wide</strong>, shared by every {@link QueryForge} instance and
 * every thread, because {@link Transport} is itself static. Separate JVMs each have their own.
 *
 * <p>When unset — the default — {@link #acquire} returns a shared no-op slot after one cached
 * check, so existing callers pay nothing measurable and send a byte-identical request.
 */
final class ProcessLimiter {

    /** Environment variable holding the cap. Identical in the Python SDK. */
    static final String MAX_CONCURRENT_ENV_VAR = "QUERYFORGE_MAX_CONCURRENT_PROCESSES";

    /** System property equivalent; like {@code queryforge.binary}, it takes precedence. */
    static final String MAX_CONCURRENT_PROPERTY = "queryforge.maxConcurrentProcesses";

    /** Code of the error raised when no slot frees up in time. Produced by the SDK, never the engine. */
    static final String BUSY_CODE = "SDK_BUSY";

    /** The slot every call gets when there is no cap. Holds nothing, so sharing it is safe. */
    private static final Slot UNLIMITED = new Slot(null, null);

    /**
     * Parsed configuration, built on first use and then reused. Volatile so the hot path can read
     * it without locking; the build itself is synchronized so it happens once.
     */
    private static volatile State state;

    private ProcessLimiter() {}

    /**
     * Waits for a slot, honouring the caller's deadline.
     *
     * <p>With no cap this returns immediately. With a cap it blocks — inside the semaphore, not in a
     * polling loop — until a slot frees up or {@code timeoutMillis} passes.
     *
     * @param timeoutMillis the caller's whole deadline, or null for none (wait as long as it takes)
     * @throws InvalidConfigException when the configured value is not a valid cap — on every call
     * @throws TimeoutException with code {@code SDK_BUSY} when the deadline passes in the queue
     * @throws ProtocolException when the thread is interrupted while queued; the flag is restored
     */
    static Slot acquire(Long timeoutMillis) {
        State s = load();
        if (s.error != null) {
            // Thrown on every call rather than once: an error reported only for the first request
            // would be lost in whichever log that one request happened to reach.
            throw new InvalidConfigException(s.error, "INVALID_CONFIG", Collections.emptyList());
        }
        if (s.semaphore == null) {
            return UNLIMITED;
        }

        long started = System.nanoTime();
        try {
            if (timeoutMillis == null) {
                s.semaphore.acquire();
            } else if (!s.semaphore.tryAcquire(timeoutMillis, TimeUnit.MILLISECONDS)) {
                throw busyError(timeoutMillis);
            }
        } catch (InterruptedException e) {
            // Restore the flag rather than swallowing it: the caller's shutdown logic depends on
            // seeing that this thread was interrupted. No permit was taken, so none is owed back.
            // ProtocolException matches what an interrupt during the subprocess wait already throws.
            Thread.currentThread().interrupt();
            throw new ProtocolException("Interrupted while waiting for a QueryForge engine slot", e);
        }
        return new Slot(s.semaphore, (System.nanoTime() - started) / 1_000_000L);
    }

    /** Builds the {@code SDK_BUSY} error for a caller whose deadline ran out in the queue. */
    static TimeoutException busyError(long timeoutMillis) {
        State s = state;
        return new TimeoutException(
                "No QueryForge engine slot became free within " + timeoutMillis + "ms: all "
                        + (s == null ? "?" : String.valueOf(s.limit)) + " slots allowed by "
                        + MAX_CONCURRENT_ENV_VAR + " stayed busy. Raise the limit, raise the timeout, "
                        + "or reduce concurrent calls.",
                BUSY_CODE,
                Collections.emptyList());
    }

    /** Returns the parsed state, reading the property and environment on first use only. */
    private static State load() {
        State s = state;
        if (s == null) {
            synchronized (ProcessLimiter.class) {
                s = state;
                if (s == null) {
                    s = parse(BinaryResolver.override(MAX_CONCURRENT_PROPERTY, MAX_CONCURRENT_ENV_VAR));
                    state = s;
                }
            }
        }
        return s;
    }

    /**
     * Turns the raw setting into state. Package-private so the parsing rules can be tested without
     * an environment variable, which a running JVM cannot set.
     *
     * <p>Deliberately parsed here and not in a static initializer: a bad value thrown from class
     * initialization becomes {@code ExceptionInInitializerError} once and {@code
     * NoClassDefFoundError} on every later use, which names nothing the user can act on.
     */
    static State parse(String raw) {
        String value = raw == null ? "" : raw.trim();
        if (value.isEmpty()) {
            // Unset and set-but-blank mean the same, as for QUERYFORGE_BINARY and the log level.
            return new State(0, null, null);
        }
        // Digits only, then range-checked. Integer.parseInt alone would accept a leading '+' and
        // non-ASCII digits; the Python SDK applies exactly this rule so a value means the same in both.
        boolean digits = true;
        for (int i = 0; i < value.length(); i++) {
            char ch = value.charAt(i);
            if (ch < '0' || ch > '9') {
                digits = false;
                break;
            }
        }
        long parsed = -1;
        if (digits) {
            try {
                parsed = Long.parseLong(value);
            } catch (NumberFormatException tooLong) {
                parsed = -1; // more digits than a long holds: out of range either way
            }
        }
        if (parsed < 1 || parsed > Integer.MAX_VALUE) {
            // Refuse rather than treat as unlimited: someone who set a cap and mistyped it believes
            // they are protected, and silently not being protected is worse than a loud error.
            return new State(0, null,
                    MAX_CONCURRENT_ENV_VAR + " (or -D" + MAX_CONCURRENT_PROPERTY + ") must be a whole "
                            + "number between 1 and " + Integer.MAX_VALUE + ", got '" + value + "'. "
                            + "Unset it to run without a limit.");
        }
        // Fair, so callers are served roughly in arrival order and a busy JVM cannot starve one.
        return new State((int) parsed, new Semaphore((int) parsed, true), null);
    }

    /** Forgets the parsed state so the next call re-reads it. Exists for tests. */
    static void resetForTests() {
        synchronized (ProcessLimiter.class) {
            state = null;
        }
    }

    /** Slots currently free, or -1 when uncapped or invalid. Exists for tests. */
    static int availableForTests() {
        State s = state;
        return (s == null || s.semaphore == null) ? -1 : s.semaphore.availablePermits();
    }

    /** Immutable parsed configuration. */
    static final class State {
        final int limit;
        final Semaphore semaphore; // null when uncapped or invalid
        final String error; // non-null when the configured value is invalid

        State(int limit, Semaphore semaphore, String error) {
            this.limit = limit;
            this.semaphore = semaphore;
            this.error = error;
        }
    }

    /** A held permit. {@link #release} must run in {@code finally}; calling it twice is harmless. */
    static final class Slot {
        private final Semaphore semaphore;
        private final Long waitMillis;
        private final AtomicBoolean released = new AtomicBoolean();

        Slot(Semaphore semaphore, Long waitMillis) {
            this.semaphore = semaphore;
            this.waitMillis = waitMillis;
        }

        /** Milliseconds spent queued, or null when uncapped so the log field is omitted, not 0. */
        Long waitMillis() {
            return waitMillis;
        }

        /**
         * Gives the permit back, once. {@link Semaphore} has no upper bound, so an unguarded second
         * release would silently raise the cap by one for the rest of the JVM's life.
         */
        void release() {
            if (semaphore != null && released.compareAndSet(false, true)) {
                semaphore.release();
            }
        }
    }
}
