package io.queryforge;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Runs the QueryForge executable and decodes its reply.
 *
 * <p>This is the whole of the SDK's communication layer: spawn the binary, write one JSON object
 * to its stdin, read one JSON object from its stdout. There is no HTTP client, no socket, no
 * daemon and no retry loop — the engine is a local subprocess with the caller's own privileges.
 *
 * <p>It is also the SDK's error boundary. Every failure below is detected, logged once at SEVERE
 * with the context needed to diagnose it, and thrown — never swallowed, never turned into an empty
 * result that a caller could mistake for an answer.
 */
final class Transport {

    /**
     * Wire protocol this SDK was built against. Only the MAJOR component is enforced: the engine
     * may add ops and optional fields (a MINOR bump) because this SDK ignores what it does not
     * recognise, but a MAJOR bump means an existing field changed meaning, and continuing would
     * produce quietly wrong output instead of an error.
     */
    static final String PROTOCOL_VERSION = "1.1";

    /**
     * Grace period added to the request's own timeout before the subprocess is destroyed. The
     * engine enforces the real deadline internally and reports it as a structured TIMEOUT, which
     * is far more useful than a killed process — so this outer bound exists only to catch an
     * engine that has hung badly enough not to honour its own deadline.
     *
     * <p>Not final: the test suite lowers it, because proving this branch works should not cost
     * fifteen seconds of every CI run.
     */
    static long killGraceMillis = 15_000L;

    private static final Logger LOG = QueryForgeLogging.logger("transport");

    private Transport() {}

    /**
     * Sends one request and returns the decoded response.
     *
     * @throws QueryForgeException when the engine reports a failure
     * @throws ProtocolException when the executable itself misbehaves
     */
    static Map<String, Object> run(Map<String, Object> request, Long timeoutMillis) {
        return run(request, timeoutMillis, null);
    }

    /**
     * As {@link #run(Map, Long)}, with a caller-supplied correlation id.
     *
     * <p>The id is stamped on every log record for this call and handed to the engine, which
     * stamps it on its own — so one {@code request_id=…} search returns both halves of a query.
     */
    static Map<String, Object> run(Map<String, Object> request, Long timeoutMillis, String requestId) {
        String rid = (requestId == null || requestId.trim().isEmpty())
                ? QueryForgeLogging.newRequestId()
                : requestId.trim();
        Map<String, Object> context = context(request, rid);

        // Ask the engine for logs at the level this SDK is itself configured for, and only then.
        // The field is omitted entirely when logging is off, which keeps the request
        // byte-identical to a protocol-1.0 one — an engine built before this field existed rejects
        // unknown fields outright, so an SDK that always sent it would break anyone pointing
        // QUERYFORGE_BINARY at an older build.
        String engineLevel = QueryForgeLogging.engineLevel();
        if (engineLevel != null) {
            request = withLogging(request, engineLevel, rid);
        }

        long started = System.nanoTime();
        try {
            Map<String, Object> response = execute(request, timeoutMillis, context);
            succeeded(context, started, response);
            return response;
        } catch (RuntimeException e) {
            // Deliberately broad, and deliberately NOT a swallow: it rethrows on every path. The
            // catch exists so that the one SEVERE line for this call is written at the boundary
            // regardless of which of the several failure kinds produced it, rather than being
            // duplicated inside each of them.
            failed(context, started, e);
            throw e;
        }
    }

    /** Runs the subprocess and decodes its reply. */
    private static Map<String, Object> execute(
            Map<String, Object> request, Long timeoutMillis, Map<String, Object> context) {
        Path binary = BinaryResolver.resolve();
        byte[] payload = Json.write(request).getBytes(StandardCharsets.UTF_8);

        QueryForgeLogging.log(LOG, Level.FINE, "sending request to the engine", context);

        ProcessBuilder builder = new ProcessBuilder(binary.toString());
        // stderr is kept separate rather than merged into stdout. Merging would be the single
        // fastest way to break every SDK at once: one diagnostic line on stderr would corrupt
        // the JSON stream that stdout is contractually required to carry alone.
        builder.redirectErrorStream(false);

        Process process;
        try {
            process = builder.start();
        } catch (IOException e) {
            throw new ProtocolException(
                    "Could not run the QueryForge executable at " + binary + ": " + e.getMessage(), e);
        }

        // stdout and stderr must be drained concurrently with the write to stdin. A subprocess
        // that fills its stdout pipe blocks until someone reads it, and if this thread is still
        // writing the request at that moment, both sides wait forever. Reading on separate
        // threads is what makes a large config safe to send.
        StreamReader stdout = new StreamReader(process.getInputStream(), "stdout");
        StreamReader stderr = new StreamReader(process.getErrorStream(), "stderr");
        stdout.start();
        stderr.start();

        // A broken pipe on the write means the process died before reading the request. The
        // useful diagnosis is on stderr, which the wait below collects — but the write error is
        // NOT discarded: it is kept and attached as the cause if this call ends up failing, so
        // "the engine produced no response" comes with the reason the request never landed.
        IOException writeFailure = null;
        try (OutputStream in = process.getOutputStream()) {
            in.write(payload);
        } catch (IOException e) {
            writeFailure = e;
            process.destroyForcibly();
        }

        long waitMillis = timeoutMillis == null ? 0L : timeoutMillis + killGraceMillis;
        int exitCode;
        try {
            if (waitMillis > 0) {
                if (!process.waitFor(waitMillis, TimeUnit.MILLISECONDS)) {
                    process.destroyForcibly();
                    throw new ProtocolException(
                            "The QueryForge executable did not respond within " + waitMillis
                                    + "ms and was killed. This is a bug in the engine — its own deadline "
                                    + "should have produced a TIMEOUT error first.");
                }
                exitCode = process.exitValue();
            } else {
                exitCode = process.waitFor();
            }
        } catch (InterruptedException e) {
            process.destroyForcibly();
            // Restore the flag rather than swallowing it: the caller's shutdown logic depends on
            // seeing that this thread was interrupted.
            Thread.currentThread().interrupt();
            throw new ProtocolException("Interrupted while waiting for the QueryForge executable", e);
        }

        String out = stdout.await();
        String err = stderr.await();
        return decode(out, err, exitCode, binary, writeFailure);
    }

    /**
     * Returns a copy of the request with the observability options set.
     *
     * <p>A copy, not a mutation: the caller's map is theirs, and a {@code QueryForge} instance
     * hands out the same options map to every query it builds.
     */
    private static Map<String, Object> withLogging(
            Map<String, Object> request, String level, String requestId) {
        Map<String, Object> copy = new LinkedHashMap<>(request);
        Map<String, Object> options = new LinkedHashMap<>();
        Object existing = request.get("options");
        if (existing instanceof Map) {
            options.putAll(Values.map(existing));
        }
        options.putIfAbsent("logLevel", level);
        options.putIfAbsent("requestId", requestId);
        copy.put("options", options);
        return copy;
    }

    /**
     * Builds the log fields for one call.
     *
     * <p>Note what goes in and what does not. The op, the backend, the entity and the SCOPE KEYS
     * go in. The question text, the scope VALUES and the config contents do not — they are
     * respectively user data, tenant identifiers, and a customer's physical schema.
     */
    private static Map<String, Object> context(Map<String, Object> request, String requestId) {
        Map<String, Object> fields = new LinkedHashMap<>();
        fields.put(QueryForgeLogging.FIELD_OPERATION, request.get("op"));
        fields.put(QueryForgeLogging.FIELD_REQUEST_ID, requestId);
        fields.put(QueryForgeLogging.FIELD_BACKEND, request.get("backend"));

        Object config = request.get("config");
        if (config instanceof Map) {
            // The entity name is the config's SHAPE, not its content — enough to tell two configs
            // apart in a log without reproducing a customer's schema.
            fields.put(QueryForgeLogging.FIELD_ENTITY, Values.map(config).get("entity"));
        }
        Object scope = request.get("scope");
        if (scope instanceof Map) {
            List<String> keys = QueryForgeLogging.scopeKeys(Values.map(scope));
            if (!keys.isEmpty()) {
                fields.put(QueryForgeLogging.FIELD_SCOPE_KEYS, keys);
            }
        }
        return fields;
    }

    /** Writes the single INFO line for a successful call. */
    private static void succeeded(
            Map<String, Object> context, long started, Map<String, Object> response) {
        if (!LOG.isLoggable(Level.INFO)) {
            return;
        }
        Map<String, Object> fields = new LinkedHashMap<>(context);
        fields.put(QueryForgeLogging.FIELD_OUTCOME, "ok");
        fields.put(QueryForgeLogging.FIELD_DURATION_MS, elapsedMillis(started));
        // A translation that succeeded on the third attempt cost three model calls, and that is
        // almost always a config gap the operator can close.
        Object repairs = response.get("repairAttempts");
        if (repairs != null) {
            fields.put("repair_attempts", repairs);
        }
        Object provider = response.get("providerUsed");
        if (provider != null) {
            fields.put("provider", provider);
        }
        QueryForgeLogging.log(LOG, Level.INFO, "engine request completed", fields);
    }

    /**
     * Writes the single SEVERE line for a failed call.
     *
     * <p>The throwable is attached, so the stack trace lands in the log exactly once — at this
     * boundary, which is the only layer that knows the whole story.
     */
    private static void failed(Map<String, Object> context, long started, RuntimeException e) {
        Map<String, Object> fields = new LinkedHashMap<>(context);
        fields.put(QueryForgeLogging.FIELD_OUTCOME, "error");
        fields.put(QueryForgeLogging.FIELD_DURATION_MS, elapsedMillis(started));
        fields.put(QueryForgeLogging.FIELD_ERROR_TYPE, e.getClass().getSimpleName());
        if (e instanceof QueryForgeException) {
            String code = ((QueryForgeException) e).getCode();
            if (!code.isEmpty()) {
                fields.put(QueryForgeLogging.FIELD_ERROR_CODE, code);
            }
        }
        QueryForgeLogging.log(LOG, Level.SEVERE, "engine request failed", fields, e);
    }

    private static long elapsedMillis(long startedNanos) {
        return (System.nanoTime() - startedNanos) / 1_000_000L;
    }

    /**
     * Turns the process output into a response map, or throws.
     *
     * <p>Note what is <em>not</em> consulted: the exit code as a success signal. The protocol's
     * contract is that stdout carries the answer, and a failed request legitimately exits non-zero
     * with a perfectly good error object on stdout. Branching on the exit code would throw that
     * structured error away and replace it with "process exited 1".
     */
    private static Map<String, Object> decode(
            String stdout, String stderr, int exitCode, Path binary, IOException writeFailure) {
        String raw = stdout == null ? "" : stdout.trim();
        if (raw.isEmpty()) {
            // Empty stdout means the process died before it could answer. stderr is the only
            // evidence, so carry it through rather than reporting a bare exit code.
            //
            // It is redacted first. stderr is the engine's text, not ours: a crash can print
            // anything, including a provider error quoting an Authorization header, and this
            // message is on its way into an exception that will be logged.
            String scrubbed = (stderr == null) ? "" : QueryForgeLogging.redact(stderr.trim());
            String detail = scrubbed.isEmpty() ? "" : " It wrote to stderr: " + scrubbed;
            String message = "The QueryForge executable at " + binary + " exited with code "
                    + exitCode + " and produced no response." + detail;
            // If writing the request failed, THAT is the root cause and this is the symptom.
            throw writeFailure == null
                    ? new ProtocolException(message)
                    : new ProtocolException(
                            message + " The request could not be written to it: "
                                    + writeFailure.getMessage(),
                            writeFailure);
        }

        Map<String, Object> response;
        try {
            response = Json.readObject(raw);
        } catch (Json.JsonException e) {
            // Truncate and scrub before quoting: a corrupted stream can be arbitrarily long and
            // can contain anything, and this message is going into someone's log.
            throw new ProtocolException(
                    "The QueryForge executable produced output that is not JSON: "
                            + QueryForgeLogging.redact(raw),
                    e);
        }

        checkProtocol(response, binary);

        if (!Values.bool(response.get("success"))) {
            throw toException(response);
        }
        return response;
    }

    /** Refuses an engine speaking an incompatible protocol major version. */
    private static void checkProtocol(Map<String, Object> response, Path binary) {
        String reported = Values.string(response.get("protocol"));
        if (reported.isEmpty()) {
            throw new ProtocolException(
                    "The executable at " + binary + " returned a response with no protocol version. "
                            + "It is probably too old for this SDK, which speaks protocol " + PROTOCOL_VERSION + ".");
        }
        if (!major(reported).equals(major(PROTOCOL_VERSION))) {
            throw new ProtocolException(
                    "Protocol mismatch: the executable at " + binary + " speaks " + reported
                            + ", this SDK speaks " + PROTOCOL_VERSION + ". Install a matching queryforge "
                            + "release, or unset " + BinaryResolver.BINARY_ENV_VAR
                            + " to use the bundled executable.");
        }
    }

    private static String major(String version) {
        int dot = version.indexOf('.');
        return dot < 0 ? version : version.substring(0, dot);
    }

    /** Builds the exception for a failed response. */
    private static QueryForgeException toException(Map<String, Object> response) {
        String code = Values.string(response.get("code"));
        String message = Values.string(response.get("message"));
        if (message.isEmpty()) {
            message = "the request failed with no message";
        }
        List<Detail> details = new ArrayList<>();
        for (Object entry : Values.list(response.get("details"))) {
            if (entry instanceof Map) {
                details.add(Detail.fromJson(Values.map(entry)));
            }
        }
        return QueryForgeException.fromCode(code, message, details);
    }

    /** Drains one stream on its own thread; see the deadlock note at the call site. */
    private static final class StreamReader extends Thread {
        private final InputStream stream;
        private final String which;
        private final ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        private volatile IOException failure;

        StreamReader(InputStream stream, String which) {
            this.stream = stream;
            this.which = which;
            setDaemon(true); // never hold up JVM shutdown over a subprocess pipe
            setName("queryforge-stream-reader-" + which);
        }

        @Override
        public void run() {
            byte[] chunk = new byte[8192];
            try {
                int read;
                while ((read = stream.read(chunk)) != -1) {
                    buffer.write(chunk, 0, read);
                }
            } catch (IOException e) {
                failure = e;
            } finally {
                try {
                    stream.close();
                } catch (IOException ignored) {
                    // Closing a pipe whose process has exited routinely fails and means nothing.
                }
            }
        }

        /**
         * Waits for the drain to finish and returns what was read.
         *
         * <p>An incomplete drain THROWS rather than returning what it managed to collect. That is a
         * change from returning the partial buffer, and it fixes a real silent failure: a truncated
         * JSON document is not obviously truncated, so the partial buffer would surface as
         * "produced output that is not JSON" — sending someone to debug the engine's encoder when
         * the actual problem was on this side of the pipe. Both ways of failing to finish are
         * covered: an I/O error, and a join that ran out of time or was interrupted while the
         * reader thread was still alive.
         */
        String await() {
            boolean interrupted = false;
            try {
                join(killGraceMillis);
            } catch (InterruptedException e) {
                interrupted = true;
                Thread.currentThread().interrupt();
            }
            if (failure != null) {
                throw new ProtocolException(
                        "Could not read the QueryForge executable's " + which + ": "
                                + failure.getMessage(),
                        failure);
            }
            if (isAlive()) {
                throw new ProtocolException(
                        "Could not read the QueryForge executable's " + which
                                + " to completion: the reader "
                                + (interrupted ? "was interrupted" : "did not finish within "
                                        + killGraceMillis + "ms")
                                + ". Any output collected so far is incomplete and has been discarded "
                                + "rather than parsed as a truncated response.");
            }
            return new String(buffer.toByteArray(), StandardCharsets.UTF_8);
        }
    }
}
