package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CyclicBarrier;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.Supplier;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

/**
 * QUERYFORGE_MAX_CONCURRENT_PROCESSES: the optional JVM-wide cap on live engine processes.
 *
 * <p>A running JVM cannot set an environment variable, so these tests use the equivalent
 * {@code -Dqueryforge.maxConcurrentProcesses} property, which goes through the same lookup
 * ({@link BinaryResolver#override}) as the variable. The parsing rules are tested directly.
 *
 * <p>Most tests run against a <em>probe</em> engine: a shell script that creates a marker directory
 * named for its PID, counts the markers present, sleeps, and removes its own marker on exit. A
 * marker exists only while its process is alive, so the largest count any probe saw can never
 * exceed the true number of simultaneous processes — "the cap held" is measured, not inferred.
 */
class QueryForgeConcurrencyTest {

    private static final String PROPERTY = ProcessLimiter.MAX_CONCURRENT_PROPERTY;

    @TempDir
    Path tmp;

    private long originalGrace;

    @BeforeEach
    void setUp() {
        assumeTrue(TestSupport.supportsShellFake(), "the probe engine needs a POSIX shell");
        originalGrace = Transport.killGraceMillis;
        System.clearProperty(PROPERTY);
        ProcessLimiter.resetForTests();
    }

    @AfterEach
    void tearDown() {
        Transport.killGraceMillis = originalGrace;
        System.clearProperty(PROPERTY);
        ProcessLimiter.resetForTests();
        TestSupport.clearBinary();
    }

    private static void cap(String value) {
        System.setProperty(PROPERTY, value);
        ProcessLimiter.resetForTests();
    }

    private static QueryForge forge() {
        return QueryForge.postgres(TestSupport.config());
    }

    // --------------------------------------------------------------------- probe engine

    /** Handle on an installed probe: what each process saw and was sent. */
    private static final class Probe {
        final Path slots;
        final Path log;

        Probe(Path slots, Path log) {
            this.slots = slots;
            this.log = log;
        }

        List<Map<String, Object>> runs() {
            try {
                if (!Files.isRegularFile(log)) {
                    return new ArrayList<>();
                }
                List<Map<String, Object>> out = new ArrayList<>();
                for (String line : Files.readAllLines(log, StandardCharsets.UTF_8)) {
                    if (!line.trim().isEmpty()) {
                        out.add(Json.readObject(line));
                    }
                }
                return out;
            } catch (IOException e) {
                throw new UncheckedIOException(e);
            }
        }

        int peak() {
            int peak = 0;
            for (Map<String, Object> run : runs()) {
                peak = Math.max(peak, ((Number) run.get("seen")).intValue());
            }
            return peak;
        }

        /** Blocks until a probe is alive — test-side polling, not SDK polling. */
        void waitUntilRunning() throws Exception {
            long deadline = System.nanoTime() + 5_000_000_000L;
            while (System.nanoTime() < deadline) {
                try (java.util.stream.Stream<Path> entries = Files.list(slots)) {
                    if (entries.findAny().isPresent()) {
                        return;
                    }
                }
                Thread.sleep(10);
            }
            throw new AssertionError("the probe process never started");
        }
    }

    private Probe probe(double sleepSeconds, String responseJson) {
        try {
            Path slots = Files.createDirectories(tmp.resolve("slots"));
            Path log = tmp.resolve("probe.jsonl");
            Path body = tmp.resolve("response.json");
            Files.write(body, responseJson.getBytes(StandardCharsets.UTF_8));
            Path script = tmp.resolve("probe.sh");
            String text = "#!/bin/sh\n"
                    + "mkdir '" + slots + "/'$$\n"
                    + "seen=$(ls '" + slots + "' | wc -l | tr -d ' ')\n"
                    + "req=$(cat)\n"
                    + "sleep " + sleepSeconds + "\n"
                    + "printf '{\"seen\":%s,\"request\":%s}\\n' \"$seen\" \"$req\" >> '" + log + "'\n"
                    + "rmdir '" + slots + "/'$$\n"
                    + "cat '" + body + "'\n";
            Files.write(script, text.getBytes(StandardCharsets.UTF_8));
            assertTrue(script.toFile().setExecutable(true, true));
            TestSupport.useBinary(script);
            return new Probe(slots, log);
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }

    private Probe probe(double sleepSeconds) {
        return probe(sleepSeconds, Json.write(TestSupport.okResponse("sql", "SELECT 1")));
    }

    /**
     * Runs {@code call} from {@code callers} threads at once and returns each result or thrown
     * exception. Daemon threads joined with a limit: if a slot ever leaks, a caller with no deadline
     * waits forever, and that must surface as a failed assertion rather than a hung build.
     */
    private static List<Object> runParallel(int callers, Supplier<Object> call) throws Exception {
        CyclicBarrier start = new CyclicBarrier(callers);
        Object[] results = new Object[callers];
        List<Thread> threads = new ArrayList<>();
        for (int i = 0; i < callers; i++) {
            final int index = i;
            Thread t = new Thread(() -> {
                try {
                    start.await();
                    results[index] = call.get();
                } catch (Throwable e) {
                    results[index] = e;
                }
            });
            t.setDaemon(true);
            threads.add(t);
            t.start();
        }
        long deadline = System.currentTimeMillis() + 30_000;
        for (Thread t : threads) {
            t.join(Math.max(1, deadline - System.currentTimeMillis()));
        }
        long stuck = threads.stream().filter(Thread::isAlive).count();
        assertEquals(0, stuck, stuck + " caller(s) never finished — a slot was not released");
        List<Object> out = new ArrayList<>();
        Collections.addAll(out, results);
        return out;
    }

    private static Thread inBackground(Runnable call) {
        Thread t = new Thread(call);
        t.setDaemon(true);
        t.start();
        return t;
    }

    private static void finish(Thread t) throws InterruptedException {
        t.join(30_000);
        assertFalse(t.isAlive(), "background call never finished — a slot was not released");
    }

    /** The {@code options.timeoutMs} a probe run was sent, or null. */
    private static Number timeoutSent(Map<String, Object> run) {
        Map<String, Object> options = Values.nullableMap(Values.map(run.get("request")).get("options"));
        return options == null ? null : (Number) options.get("timeoutMs");
    }

    /** A probe run's request with the per-call {@code options.requestId} removed. */
    private static Map<String, Object> withoutRequestId(Map<String, Object> run) {
        Map<String, Object> request = new java.util.LinkedHashMap<>(Values.map(run.get("request")));
        Map<String, Object> options = Values.nullableMap(request.get("options"));
        if (options != null) {
            options = new java.util.LinkedHashMap<>(options);
            options.remove("requestId");
            request.put("options", options);
        }
        return request;
    }

    private static void assertNoFailures(List<Object> results) {
        for (Object r : results) {
            assertFalse(r instanceof Throwable, String.valueOf(r));
        }
    }

    // ------------------------------------------------------------------ default: no cap

    @Test
    @DisplayName("unset imposes no limit")
    void unsetImposesNoLimit() throws Exception {
        Probe p = probe(0.6);
        assertNoFailures(runParallel(6, () -> forge().query("orders").toSql()));
        // Proves the probe can observe concurrency at all, so the capped tests below measure the
        // cap and not a probe that always reports 1.
        assertTrue(p.peak() >= 3, "peak was " + p.peak());
        assertEquals(-1, ProcessLimiter.availableForTests());
    }

    @Test
    @DisplayName("an uncontended capped call sends exactly the request an uncapped one does")
    void uncontendedRequestIsUnchanged() {
        Probe p = probe(0);
        forge().query("orders").timeout(2500).toSql();
        cap("1");
        forge().query("orders").timeout(2500).toSql();
        List<Map<String, Object>> runs = p.runs();
        // requestId is fresh per call when another suite left logging configured; ignore just that.
        assertEquals(withoutRequestId(runs.get(0)), withoutRequestId(runs.get(1)));
        assertEquals(2500, timeoutSent(runs.get(1)).intValue());
    }

    // ------------------------------------------------------------------ the cap holds

    @ParameterizedTest(name = "cap {0}")
    @ValueSource(ints = {1, 2, 3})
    @DisplayName("the cap bounds simultaneous processes")
    void capBoundsSimultaneousProcesses(int limit) throws Exception {
        cap(String.valueOf(limit));
        Probe p = probe(0.4);
        assertNoFailures(runParallel(7, () -> forge().query("orders").toSql()));
        assertEquals(7, p.runs().size());
        assertEquals(limit, p.peak());
        assertEquals(limit, ProcessLimiter.availableForTests());
    }

    @Test
    @DisplayName("the cap is shared by every QueryForge instance in the JVM")
    void capIsJvmWide() throws Exception {
        cap("2");
        Probe p = probe(0.4);
        // A fresh instance per call: the cap must not be per-instance.
        assertNoFailures(runParallel(6, () -> QueryForge.mysql(TestSupport.config()).query("orders").toSql()));
        assertEquals(2, p.peak());
    }

    @Test
    @DisplayName("waiting callers proceed when a slot frees")
    void waitingCallersProceed() throws Exception {
        cap("1");
        Probe p = probe(0.3);
        long started = System.nanoTime();
        List<Object> results = runParallel(3, () -> forge().query("orders").toSql());
        long elapsedMs = (System.nanoTime() - started) / 1_000_000L;
        assertEquals(Collections.nCopies(3, "SELECT 1"), results);
        assertEquals(1, p.peak());
        assertTrue(elapsedMs >= 850, "three 300ms calls must run back to back, took " + elapsedMs);
    }

    @Test
    @DisplayName("large responses do not deadlock under a cap")
    void largeResponsesDoNotDeadlock() throws Exception {
        cap("1");
        StringBuilder big = new StringBuilder("SELECT ");
        for (int i = 0; i < 4 * 1024 * 1024; i++) {
            big.append('x');
        }
        probe(0, Json.write(TestSupport.okResponse("sql", big.toString())));
        List<Object> results = runParallel(3, () -> forge().query("orders").timeout(20_000).toSql());
        assertNoFailures(results);
        for (Object r : results) {
            assertEquals(big.length(), ((String) r).length());
        }
    }

    // ------------------------------------------------------------------ deadlines

    @Test
    @DisplayName("a deadline expiring in the queue raises SDK_BUSY and starts nothing")
    void deadlineInQueueIsBusy() throws Exception {
        cap("1");
        Probe p = probe(1.5);
        Thread holder = inBackground(() -> forge().query("orders").toSql());
        p.waitUntilRunning();

        long started = System.nanoTime();
        TimeoutException e = assertThrows(
                TimeoutException.class, () -> forge().query("orders").timeout(200).toSql());
        long waitedMs = (System.nanoTime() - started) / 1_000_000L;
        finish(holder);

        assertEquals("SDK_BUSY", e.getCode());
        assertTrue(e.getMessage().contains(ProcessLimiter.MAX_CONCURRENT_ENV_VAR), e.getMessage());
        assertTrue(waitedMs >= 150 && waitedMs < 1000, "waited " + waitedMs + "ms");
        assertEquals(1, p.runs().size(), "the timed-out caller must never start a process");
    }

    @Test
    @DisplayName("queue time comes out of the engine's deadline")
    void queueTimeIsDeducted() throws Exception {
        cap("1");
        Probe p = probe(0.8);
        Thread holder = inBackground(() -> forge().query("orders").toSql());
        p.waitUntilRunning();
        forge().query("orders").timeout(5000).toSql();
        finish(holder);

        List<Integer> sent = new ArrayList<>();
        for (Map<String, Object> run : p.runs()) {
            if (timeoutSent(run) != null) {
                sent.add(timeoutSent(run).intValue());
            }
        }
        assertEquals(1, sent.size());
        assertTrue(sent.get(0) >= 3500 && sent.get(0) <= 4900, "sent " + sent.get(0));
    }

    @Test
    @DisplayName("with no deadline a caller waits as long as it takes")
    void noDeadlineWaits() throws Exception {
        cap("1");
        Probe p = probe(0.6);
        assertEquals(Collections.nCopies(2, "SELECT 1"), runParallel(2, () -> forge().query("orders").toSql()));
        for (Map<String, Object> run : p.runs()) {
            assertNull(timeoutSent(run), "no deadline was set, so none may be invented");
        }
    }

    @Test
    @DisplayName("interrupted while queued: flag restored, nothing started, no slot taken")
    void interruptWhileQueued() throws Exception {
        cap("1");
        Probe p = probe(1.5);
        Thread holder = inBackground(() -> forge().query("orders").toSql());
        p.waitUntilRunning();

        AtomicReference<Throwable> thrown = new AtomicReference<>();
        AtomicReference<Boolean> flag = new AtomicReference<>();
        Thread waiter = inBackground(() -> {
            try {
                forge().query("orders").toSql();
            } catch (Throwable e) {
                thrown.set(e);
                flag.set(Thread.currentThread().isInterrupted());
            }
        });
        Thread.sleep(200); // let it reach the queue
        waiter.interrupt();
        finish(waiter);
        finish(holder);

        assertTrue(thrown.get() instanceof ProtocolException, String.valueOf(thrown.get()));
        assertTrue(flag.get(), "the interrupt flag must be restored");
        assertEquals(1, p.runs().size());
        assertEquals(1, ProcessLimiter.availableForTests());
    }

    // ------------------------------------------------------------- permits always come back

    /** Asserts the one slot is free, both by count and by a call that would otherwise be SDK_BUSY. */
    private void assertSlotReturned() {
        assertEquals(1, ProcessLimiter.availableForTests());
        TestSupport.useBinary(TestSupport.fakeBinary(tmp.resolve("after"),
                TestSupport.okResponse("sql", "SELECT 2")).path());
        assertEquals("SELECT 2", forge().query("orders").timeout(2000).toSql());
    }

    @Test
    @DisplayName("slot is released after an engine error response")
    void releasedAfterErrorResponse() {
        cap("1");
        TestSupport.useBinary(TestSupport.fakeBinary(tmp, TestSupport.errResponse("VALIDATION_FAILED", "no")).path());
        assertThrows(ValidationException.class, () -> forge().query("orders").timeout(2000).toSql());
        assertSlotReturned();
    }

    @ParameterizedTest(name = "{0}")
    @ValueSource(strings = {"crash", "garbage", "mismatch", "kill"})
    @DisplayName("slot is released after a broken engine")
    void releasedAfterBrokenEngine(String kind) {
        cap("1");
        String ok = Json.write(TestSupport.okResponse("sql", "SELECT 1"));
        long timeout = 2000;
        TestSupport.FakeBinary fake;
        switch (kind) {
            case "crash":
                fake = TestSupport.fakeBinary(tmp, null, 2, "panic: boom", 0);
                break;
            case "garbage":
                fake = TestSupport.fakeBinary(tmp, "not json at all", 0, "", 0);
                break;
            case "mismatch":
                fake = TestSupport.fakeBinary(tmp, Json.write(TestSupport.okResponse("protocol", "9.0")), 0, "", 0);
                break;
            default:
                Transport.killGraceMillis = 200;
                timeout = 50;
                fake = TestSupport.fakeBinary(tmp, ok, 0, "", 10_000);
        }
        TestSupport.useBinary(fake.path());
        final long t = timeout;
        assertThrows(ProtocolException.class, () -> forge().query("orders").timeout(t).toSql());
        Transport.killGraceMillis = originalGrace;
        assertSlotReturned();
    }

    @Test
    @DisplayName("slot is released when the executable cannot be started")
    void releasedAfterStartFailure() throws Exception {
        cap("1");
        // Executable, but its interpreter does not exist: it resolves fine, then exec fails with
        // ENOENT inside ProcessBuilder.start. (Random bytes would not do: the JDK retries an
        // ENOEXEC file through /bin/sh, which runs and exits 126 instead of failing to start.)
        Path bogus = tmp.resolve("bogus");
        Files.write(bogus, "#!/nonexistent/interpreter\n".getBytes(StandardCharsets.UTF_8));
        assertTrue(bogus.toFile().setExecutable(true, true));
        TestSupport.useBinary(bogus);
        ProtocolException e = assertThrows(
                ProtocolException.class, () -> forge().query("orders").timeout(2000).toSql());
        assertTrue(e.getMessage().contains("Could not run"), e.getMessage());
        assertSlotReturned();
    }

    @Test
    @DisplayName("slot is released when interrupted while the engine runs")
    void releasedAfterInterruptDuringRun() throws Exception {
        cap("1");
        Probe p = probe(5);
        AtomicReference<Throwable> thrown = new AtomicReference<>();
        Thread caller = inBackground(() -> {
            try {
                forge().query("orders").toSql();
            } catch (Throwable e) {
                thrown.set(e);
            }
        });
        p.waitUntilRunning();
        caller.interrupt();
        finish(caller);
        assertTrue(thrown.get() instanceof ProtocolException, String.valueOf(thrown.get()));
        assertSlotReturned();
    }

    @Test
    @DisplayName("releasing a slot twice cannot raise the cap")
    void doubleReleaseIsHarmless() {
        cap("1");
        ProcessLimiter.Slot slot = ProcessLimiter.acquire(null);
        slot.release();
        slot.release();
        assertEquals(1, ProcessLimiter.availableForTests());
    }

    // ------------------------------------------------------------------ configuration

    @ParameterizedTest(name = "\"{0}\"")
    @ValueSource(strings = {"0", "-1", "abc", "1.5", "8_0", "+3", "2147483648", "99999999999999999999",
            "８", "4 slots", "0x10"})
    @DisplayName("invalid values fail clearly on every call and start nothing")
    void invalidValuesFail(String bad) {
        cap(bad);
        TestSupport.FakeBinary fake = TestSupport.fakeBinary(tmp, TestSupport.okResponse("sql", "SELECT 1"));
        TestSupport.useBinary(fake.path());
        for (int i = 0; i < 2; i++) {
            InvalidConfigException e = assertThrows(
                    InvalidConfigException.class, () -> forge().query("orders").toSql());
            assertEquals("INVALID_CONFIG", e.getCode());
            assertTrue(e.getMessage().contains(ProcessLimiter.MAX_CONCURRENT_ENV_VAR), e.getMessage());
        }
        assertEquals(0, fake.callCount());
    }

    @ParameterizedTest(name = "\"{0}\"")
    @ValueSource(strings = {"", "   "})
    @DisplayName("a blank value means no limit")
    void blankMeansNoLimit(String blank) {
        assertNull(ProcessLimiter.parse(blank).semaphore);
        assertNull(ProcessLimiter.parse(blank).error);
        assertNull(ProcessLimiter.parse(null).semaphore);
    }

    @Test
    @DisplayName("surrounding whitespace and leading zeros are accepted")
    void whitespaceAndLeadingZeros() {
        assertEquals(4, ProcessLimiter.parse(" 04 ").limit);
        assertEquals(Integer.MAX_VALUE, ProcessLimiter.parse("2147483647").limit);
    }

    @Test
    @DisplayName("the value is read once")
    void readOnce() {
        cap("2");
        TestSupport.useBinary(TestSupport.fakeBinary(tmp, TestSupport.okResponse("sql", "SELECT 1")).path());
        forge().query("orders").toSql();
        System.setProperty(PROPERTY, "not a number"); // no reset: a live JVM keeps its parsed cap
        assertEquals("SELECT 1", forge().query("orders").toSql());
        assertEquals(2, ProcessLimiter.availableForTests());
    }

    // ------------------------------------------------------------------ logging

    /** Captures the SDK's records for one test. */
    private static final class Recorder extends Handler {
        final List<LogRecord> records = Collections.synchronizedList(new ArrayList<>());

        @Override
        public void publish(LogRecord record) {
            records.add(record);
        }

        @Override
        public void flush() {}

        @Override
        public void close() {}
    }

    private static <T> T withRecorder(java.util.function.Function<Recorder, T> body) {
        Logger root = Logger.getLogger(QueryForgeLogging.LOGGER_NAME);
        Level level = root.getLevel();
        boolean parents = root.getUseParentHandlers();
        Recorder recorder = new Recorder();
        recorder.setLevel(Level.ALL);
        root.addHandler(recorder);
        root.setLevel(Level.FINE);
        root.setUseParentHandlers(false);
        try {
            return body.apply(recorder);
        } finally {
            root.removeHandler(recorder);
            root.setLevel(level);
            root.setUseParentHandlers(parents);
        }
    }

    @Test
    @DisplayName("capped calls log their queue time; the scope value never appears")
    void cappedCallsLogWaitMs() {
        cap("1");
        probe(0.3);
        Map<String, Object> scope = Collections.singletonMap("tenantId", "t-SECRET-42");
        List<Map<String, Object>> done = withRecorder(rec -> {
            try {
                runParallel(2, () -> forge().withScope(scope).query("orders").toSql());
            } catch (Exception e) {
                throw new RuntimeException(e);
            }
            List<Map<String, Object>> fields = new ArrayList<>();
            StringBuilder text = new StringBuilder();
            for (LogRecord r : new ArrayList<>(rec.records)) {
                Map<String, Object> f = QueryForgeLogging.fieldsOf(r);
                text.append(r.getMessage()).append(f);
                if ("ok".equals(f.get("outcome"))) {
                    fields.add(f);
                }
            }
            assertFalse(text.toString().contains("t-SECRET-42"));
            return fields;
        });
        assertEquals(2, done.size());
        List<Long> waits = new ArrayList<>();
        for (Map<String, Object> f : done) {
            long wait = ((Number) f.get("wait_ms")).longValue();
            waits.add(wait);
            assertTrue(((Number) f.get("duration_ms")).longValue() >= wait, "duration_ms includes the queue");
        }
        Collections.sort(waits);
        assertTrue(waits.get(0) < 200 && waits.get(1) >= 200, "waits " + waits);
    }

    @Test
    @DisplayName("uncapped calls have no wait_ms field")
    void uncappedHasNoWaitField() {
        TestSupport.useBinary(TestSupport.fakeBinary(tmp, TestSupport.okResponse("sql", "SELECT 1")).path());
        List<LogRecord> records = withRecorder(rec -> {
            forge().query("orders").toSql();
            return new ArrayList<>(rec.records);
        });
        assertFalse(records.isEmpty());
        for (LogRecord r : records) {
            assertFalse(QueryForgeLogging.fieldsOf(r).containsKey("wait_ms"));
        }
    }

    @Test
    @DisplayName("SDK_BUSY is logged once at SEVERE with its code")
    void busyLoggedOnce() throws Exception {
        cap("1");
        Probe p = probe(1.0);
        Thread holder = inBackground(() -> forge().query("orders").toSql());
        p.waitUntilRunning();
        List<LogRecord> severe = withRecorder(rec -> {
            assertThrows(TimeoutException.class, () -> forge().query("orders").timeout(100).toSql());
            List<LogRecord> out = new ArrayList<>();
            for (LogRecord r : new ArrayList<>(rec.records)) {
                if (r.getLevel().intValue() >= Level.SEVERE.intValue()) {
                    out.add(r);
                }
            }
            return out;
        });
        finish(holder);
        assertEquals(1, severe.size());
        assertEquals("SDK_BUSY", QueryForgeLogging.fieldsOf(severe.get(0)).get("error_code"));
    }

    // ------------------------------------------------------------------ real engine

    @Test
    @DisplayName("real engine: results are identical under a cap")
    void realEngineResultsUnchanged() throws Exception {
        Path engine = TestSupport.engineBinary();
        assumeTrue(engine != null, "no engine binary and no Go toolchain");
        TestSupport.useBinary(engine);
        QueryForgeResult expected = forge().generate(TestSupport.validAst());

        cap("2");
        List<Object> results = runParallel(8, () -> forge().generate(TestSupport.validAst()));
        assertNoFailures(results);
        for (Object r : results) {
            QueryForgeResult result = (QueryForgeResult) r;
            assertEquals(expected.getSql(), result.getSql());
            assertEquals(expected.getArgs(), result.getArgs());
        }
    }
}
