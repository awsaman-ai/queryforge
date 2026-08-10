package io.queryforge;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Fixtures shared by the Java test suites.
 *
 * <p>The suite is split deliberately between two kinds of test: those that run against the
 * <strong>real engine binary</strong> using only its offline ops (which prove the SDK and the
 * engine actually agree — the thing a mock cannot tell you), and those that run against a
 * <strong>scripted fake</strong> (which cover every error code, a crash, corrupted output and a
 * protocol mismatch, none of which the real engine can be asked to produce on demand).
 */
final class TestSupport {

    private TestSupport() {}

    /** Repository root, derived from this module's location. */
    static Path repoRoot() {
        return Paths.get("").toAbsolutePath().getParent();
    }

    /**
     * Locates the real engine binary, building it if a Go toolchain is available.
     *
     * <p>Returns null when neither is possible, so the integration tests can skip cleanly rather
     * than fail on a machine that has no Go.
     */
    static Path engineBinary() {
        Path bundled = Paths.get("").toAbsolutePath()
                .resolve("target/test-bin/queryforge");
        if (Files.isRegularFile(bundled) && Files.isExecutable(bundled)) {
            return bundled;
        }
        try {
            Files.createDirectories(bundled.getParent());
            Process build = new ProcessBuilder(
                            "go", "build", "-o", bundled.toString(), "./cmd/queryforge")
                    .directory(repoRoot().toFile())
                    .redirectErrorStream(true)
                    .start();
            if (build.waitFor() != 0) {
                return null;
            }
            return Files.isRegularFile(bundled) ? bundled : null;
        } catch (IOException | InterruptedException e) {
            if (e instanceof InterruptedException) {
                Thread.currentThread().interrupt();
            }
            return null;
        }
    }

    /** Points the SDK at a specific executable for the current test. */
    static void useBinary(Path binary) {
        System.setProperty(BinaryResolver.BINARY_PROPERTY, binary.toString());
        BinaryResolver.resetCache();
    }

    /** Clears any binary override. */
    static void clearBinary() {
        System.clearProperty(BinaryResolver.BINARY_PROPERTY);
        BinaryResolver.resetCache();
    }

    /**
     * A config exercising the shapes the protocol must carry: an enum with a domain, a
     * non-returnable field, per-backend physical name mappings, and both a SQL and a Mongo
     * binding. Written inline so editing a shipped example cannot change what the tests assert.
     */
    static Map<String, Object> config() {
        return QueryForge.parseConfig(
                "{"
                        + "\"entity\": \"Order\","
                        + "\"model\": {\"provider\":\"stub\",\"baseURL\":\"http://localhost\",\"model\":\"test\"},"
                        + "\"backends\": {\"sql\":{\"table\":\"orders\"},\"mysql\":{\"table\":\"orders\"},"
                        + "\"mongo\":{\"collection\":\"orders\"}},"
                        + "\"fields\": ["
                        + "{\"name\":\"status\",\"type\":\"enum\",\"values\":[\"NEW\",\"DELIVERED\",\"CANCELLED\"],\"indexed\":true},"
                        + "{\"name\":\"amount\",\"type\":\"number\",\"mapping\":{\"sql\":\"total_amount\",\"mysql\":\"total_amount\"}},"
                        + "{\"name\":\"customerName\",\"type\":\"string\",\"mapping\":{\"sql\":\"customer_name\",\"mysql\":\"customer_name\"}},"
                        + "{\"name\":\"internalNote\",\"type\":\"string\",\"returnable\":false}"
                        + "]}");
    }

    /** A legal AST for {@link #config()}: status = DELIVERED. */
    static Map<String, Object> validAst() {
        return QueryForge.parseConfig(
                "{\"version\":\"1.0\",\"entity\":\"Order\",\"filter\":{\"type\":\"comparison\","
                        + "\"field\":\"status\",\"operator\":\"equals\",\"value\":{\"kind\":\"enum\",\"v\":\"DELIVERED\"}}}");
    }

    /** An AST naming a field the config does not register. */
    static Map<String, Object> astWithUnknownField() {
        return QueryForge.parseConfig(
                "{\"version\":\"1.0\",\"entity\":\"Order\",\"filter\":{\"type\":\"comparison\","
                        + "\"field\":\"amont\",\"operator\":\"gt\",\"value\":{\"kind\":\"number\",\"v\":100}}}");
    }

    /** Builds a successful response envelope with sensible defaults. */
    static Map<String, Object> okResponse(Object... keyValuePairs) {
        Map<String, Object> base = new LinkedHashMap<>();
        base.put("success", Boolean.TRUE);
        base.put("protocol", "1.0");
        base.put("op", "translate");
        base.put("backend", "sql");
        putPairs(base, keyValuePairs);
        return base;
    }

    /** Builds a failed response envelope. */
    static Map<String, Object> errResponse(String code, String message, Object... keyValuePairs) {
        Map<String, Object> base = new LinkedHashMap<>();
        base.put("success", Boolean.FALSE);
        base.put("protocol", "1.0");
        base.put("code", code);
        base.put("message", message);
        putPairs(base, keyValuePairs);
        return base;
    }

    private static void putPairs(Map<String, Object> target, Object... pairs) {
        for (int i = 0; i + 1 < pairs.length; i += 2) {
            target.put((String) pairs[i], pairs[i + 1]);
        }
    }

    /** A scripted stand-in for the engine executable. */
    static final class FakeBinary {
        private final Path launcher;
        private final Path log;

        FakeBinary(Path launcher, Path log) {
            this.launcher = launcher;
            this.log = log;
        }

        Path path() {
            return launcher;
        }

        /** The requests this fake received, in order. */
        List<Map<String, Object>> invocations() {
            if (!Files.isRegularFile(log)) {
                return new ArrayList<>();
            }
            try {
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

        int callCount() {
            return invocations().size();
        }
    }

    /**
     * Installs a scripted fake engine that echoes {@code responseBody} and logs what it was sent.
     *
     * @param responseBody raw text to write to stdout — usually JSON, sometimes deliberately not
     * @param exitCode the exit status to report
     * @param stderr text to write to stderr
     * @param sleepMillis how long to stall before answering, for the hung-engine case
     */
    static FakeBinary fakeBinary(
            Path dir, String responseBody, int exitCode, String stderr, long sleepMillis) {
        try {
            Files.createDirectories(dir);
            Path log = dir.resolve("invocations.jsonl");
            Path script = dir.resolve("fake_queryforge.sh");

            // A POSIX shell script rather than a Java program: it needs no compilation step and
            // no second JVM, so a test that spawns it costs milliseconds.
            String body =
                    "#!/bin/sh\n"
                            + "sleep " + (sleepMillis / 1000.0) + "\n"
                            + "cat >> '" + log + "'\n"
                            + "printf '\\n' >> '" + log + "'\n"
                            + (stderr.isEmpty() ? "" : "printf '%s' " + shellQuote(stderr) + " >&2\n")
                            + (responseBody == null ? "" : "printf '%s' " + shellQuote(responseBody) + "\n")
                            + "exit " + exitCode + "\n";
            Files.write(script, body.getBytes(StandardCharsets.UTF_8));
            if (!script.toFile().setExecutable(true, true)) {
                throw new IOException("could not mark the fake binary executable");
            }
            return new FakeBinary(script, log);
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }

    /** Convenience overload for the common case: answer this JSON, exit 0. */
    static FakeBinary fakeBinary(Path dir, Map<String, Object> response) {
        return fakeBinary(dir, Json.write(response), 0, "", 0);
    }

    /** Wraps a string in single quotes for /bin/sh, escaping any it contains. */
    private static String shellQuote(String s) {
        return "'" + s.replace("'", "'\\''") + "'";
    }

    /** True when the platform can run the shell-script fake. */
    static boolean supportsShellFake() {
        return !System.getProperty("os.name", "").toLowerCase().contains("win");
    }

    /** Convenience: the backends the shipped engine registers. */
    static final List<String> BACKENDS = Arrays.asList("sql", "mysql", "mongo");
}
