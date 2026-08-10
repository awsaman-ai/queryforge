package io.queryforge;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/**
 * Runs the QueryForge executable and decodes its reply.
 *
 * <p>This is the whole of the SDK's communication layer: spawn the binary, write one JSON object
 * to its stdin, read one JSON object from its stdout. There is no HTTP client, no socket, no
 * daemon and no retry loop — the engine is a local subprocess with the caller's own privileges.
 */
final class Transport {

    /**
     * Wire protocol this SDK was built against. Only the MAJOR component is enforced: the engine
     * may add ops and optional fields (a MINOR bump) because this SDK ignores what it does not
     * recognise, but a MAJOR bump means an existing field changed meaning, and continuing would
     * produce quietly wrong output instead of an error.
     */
    static final String PROTOCOL_VERSION = "1.0";

    /**
     * Grace period added to the request's own timeout before the subprocess is destroyed. The
     * engine enforces the real deadline internally and reports it as a structured TIMEOUT, which
     * is far more useful than a killed process — so this outer bound exists only to catch an
     * engine that has hung badly enough not to honour its own deadline.
     */
    private static final long KILL_GRACE_MILLIS = 15_000L;

    private Transport() {}

    /**
     * Sends one request and returns the decoded response.
     *
     * @throws QueryForgeException when the engine reports a failure
     * @throws ProtocolException when the executable itself misbehaves
     */
    static Map<String, Object> run(Map<String, Object> request, Long timeoutMillis) {
        Path binary = BinaryResolver.resolve();
        byte[] payload = Json.write(request).getBytes(StandardCharsets.UTF_8);

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
        StreamReader stdout = new StreamReader(process.getInputStream());
        StreamReader stderr = new StreamReader(process.getErrorStream());
        stdout.start();
        stderr.start();

        try (OutputStream in = process.getOutputStream()) {
            in.write(payload);
        } catch (IOException e) {
            // A broken pipe here means the process died before reading the request. Do not report
            // the write error — the useful diagnosis is on stderr, which the wait below collects.
            process.destroyForcibly();
        }

        long waitMillis = timeoutMillis == null ? 0L : timeoutMillis + KILL_GRACE_MILLIS;
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
        return decode(out, err, exitCode, binary);
    }

    /**
     * Turns the process output into a response map, or throws.
     *
     * <p>Note what is <em>not</em> consulted: the exit code as a success signal. The protocol's
     * contract is that stdout carries the answer, and a failed request legitimately exits non-zero
     * with a perfectly good error object on stdout. Branching on the exit code would throw that
     * structured error away and replace it with "process exited 1".
     */
    private static Map<String, Object> decode(String stdout, String stderr, int exitCode, Path binary) {
        String raw = stdout == null ? "" : stdout.trim();
        if (raw.isEmpty()) {
            // Empty stdout means the process died before it could answer. stderr is the only
            // evidence, so carry it through rather than reporting a bare exit code.
            String detail = (stderr == null || stderr.trim().isEmpty())
                    ? ""
                    : " It wrote to stderr: " + stderr.trim();
            throw new ProtocolException(
                    "The QueryForge executable at " + binary + " exited with code " + exitCode
                            + " and produced no response." + detail);
        }

        Map<String, Object> response;
        try {
            response = Json.readObject(raw);
        } catch (Json.JsonException e) {
            // Truncate before quoting: a corrupted stream can be arbitrarily long, and this
            // message is going into someone's log.
            String excerpt = raw.length() > 500 ? raw.substring(0, 500) + "…" : raw;
            throw new ProtocolException(
                    "The QueryForge executable produced output that is not JSON: " + excerpt, e);
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
        private final ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        private volatile IOException failure;

        StreamReader(InputStream stream) {
            this.stream = stream;
            setDaemon(true); // never hold up JVM shutdown over a subprocess pipe
            setName("queryforge-stream-reader");
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

        /** Waits for the drain to finish and returns what was read. */
        String await() {
            try {
                join(KILL_GRACE_MILLIS);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
            if (failure != null) {
                throw new ProtocolException(
                        "Could not read from the QueryForge executable: " + failure.getMessage(), failure);
            }
            return new String(buffer.toByteArray(), StandardCharsets.UTF_8);
        }
    }
}
