package io.queryforge;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.security.MessageDigest;
import java.util.Locale;

/**
 * Finds the QueryForge executable, extracting it from the jar when necessary.
 *
 * <p>Java differs from Python here in one way that shapes everything below: a jar entry is not a
 * file, so the binary cannot be executed where it sits. It has to be copied out to a real path
 * first. Doing that on every call would add a multi-megabyte write to every query, so the
 * extracted copy is cached on disk under a content-addressed name and reused.
 *
 * <p>Content-addressing — the name carries a hash of the bytes — is what makes the cache safe to
 * share. Two applications using different QueryForge versions on one machine get different cache
 * entries rather than one silently overwriting the other, and an upgrade cannot be defeated by a
 * stale file that happens to have the right name.
 */
final class BinaryResolver {

    /** Overrides the search entirely; see the SDK README. */
    static final String BINARY_ENV_VAR = "QUERYFORGE_BINARY";

    /**
     * System property equivalent of {@link #BINARY_ENV_VAR}, and the one that wins.
     *
     * <p>Both forms exist because they serve different people. The environment variable is what a
     * container image or a shell profile sets, and is the same name every QueryForge SDK honours.
     * The system property is what a JVM application configures — {@code -Dqueryforge.binary=…} in
     * a launch script, or {@code System.setProperty} in a test — and Java has no supported way to
     * set an environment variable for its own process, so without it a JVM-only caller could not
     * redirect the binary at all.
     */
    static final String BINARY_PROPERTY = "queryforge.binary";

    /** Overrides where extracted binaries are cached. */
    static final String CACHE_DIR_ENV_VAR = "QUERYFORGE_CACHE_DIR";

    /** System property equivalent of {@link #CACHE_DIR_ENV_VAR}, and the one that wins. */
    static final String CACHE_DIR_PROPERTY = "queryforge.cacheDir";

    /** Resolved at most once per class loader — see {@link #resolve()}. */
    private static volatile Path cached;

    private BinaryResolver() {}

    /**
     * Returns the executable to run, resolving and extracting it at most once.
     *
     * <p>The cache is skipped whenever an override is set, so a test can repoint it between calls
     * without a stale hit.
     */
    static Path resolve() {
        String override = override(BINARY_PROPERTY, BINARY_ENV_VAR);
        if (override != null) {
            return fromOverride(override);
        }
        Path local = cached;
        if (local != null && Files.isRegularFile(local)) {
            return local;
        }
        synchronized (BinaryResolver.class) {
            if (cached == null || !Files.isRegularFile(cached)) {
                cached = extractFromJar();
            }
            return cached;
        }
    }

    /**
     * Returns the first non-blank of a system property and an environment variable, or null.
     *
     * <p>The property wins so a JVM launch flag can override an environment inherited from a
     * container image, which is the direction that is almost always intended.
     */
    private static String override(String property, String envVar) {
        String value = System.getProperty(property);
        if (value == null || value.trim().isEmpty()) {
            value = System.getenv(envVar);
        }
        return (value == null || value.trim().isEmpty()) ? null : value.trim();
    }

    /** Honours an explicit override, reporting rather than falling back when it does not work. */
    private static Path fromOverride(String value) {
        // Falling back to the bundled binary here would run a *different* engine than the one the
        // user named, which is precisely what this variable exists to prevent.
        Path path = Paths.get(value);
        if (!Files.isRegularFile(path)) {
            throw new BinaryNotFoundException(
                    "The QueryForge binary override points at '" + value + "', which is not a file. "
                            + "It was set via -D" + BINARY_PROPERTY + " or " + BINARY_ENV_VAR + ".");
        }
        if (!Files.isExecutable(path)) {
            throw new BinaryNotFoundException(
                    "The QueryForge binary override points at '" + value + "', which is not executable. "
                            + "Try: chmod +x " + value);
        }
        return path;
    }

    /**
     * Returns the {@code <os>-<arch>} tag for the running JVM.
     *
     * <p>The tags match the release artifact names, so the string in an error message is the same
     * string the user will look for on the releases page.
     */
    static String platformTag() {
        String osName = System.getProperty("os.name", "").toLowerCase(Locale.ROOT);
        String os;
        if (osName.contains("linux")) {
            os = "linux";
        } else if (osName.contains("mac") || osName.contains("darwin")) {
            os = "darwin";
        } else if (osName.contains("win")) {
            os = "windows";
        } else {
            throw new BinaryNotFoundException(
                    "QueryForge has no build for this operating system ('" + osName + "'). "
                            + "Supported: Linux, macOS, Windows. Set " + BINARY_ENV_VAR
                            + " to point at a binary you built yourself.");
        }

        // os.arch reports the JVM's architecture, which is the one that matters: a 32-bit JVM on
        // a 64-bit machine must still get a binary it can exec. The aliases are all real —
        // "x86_64" on macOS, "amd64" on Linux and Windows, "aarch64" vs "arm64" across vendors.
        String archName = System.getProperty("os.arch", "").toLowerCase(Locale.ROOT);
        String arch;
        if (archName.equals("amd64") || archName.equals("x86_64") || archName.equals("x64")) {
            arch = "amd64";
        } else if (archName.equals("aarch64") || archName.equals("arm64")) {
            arch = "arm64";
        } else {
            throw new BinaryNotFoundException(
                    "QueryForge has no build for this CPU architecture ('" + archName + "'). "
                            + "Supported: amd64 (x86_64) and arm64 (aarch64). Set " + BINARY_ENV_VAR
                            + " to point at a binary you built yourself.");
        }
        return os + "-" + arch;
    }

    private static String executableName() {
        return platformTag().startsWith("windows") ? "queryforge.exe" : "queryforge";
    }

    /** Copies the bundled executable out of the jar into a cached location on disk. */
    private static Path extractFromJar() {
        String tag = platformTag();
        String resource = "/io/queryforge/bin/" + tag + "/" + executableName();

        try (InputStream in = BinaryResolver.class.getResourceAsStream(resource)) {
            if (in == null) {
                throw new BinaryNotFoundException(
                        "No QueryForge executable is bundled for this platform (" + tag + "). "
                                + "Looked for '" + resource + "' on the classpath. If you are using the "
                                + "platform-specific artifact, check that its classifier matches this machine; "
                                + "otherwise set " + BINARY_ENV_VAR + " to a binary you built yourself.");
            }

            byte[] bytes = readAll(in);
            Path target = cacheLocation(tag, bytes);

            // Reuse an intact extraction. Comparing the size is enough given the name already
            // carries a hash of the contents: a matching name plus a matching length means the
            // same bytes, and a full re-read on every startup would cost more than it protects.
            if (Files.isRegularFile(target) && Files.size(target) == bytes.length) {
                ensureExecutable(target);
                return target;
            }

            Files.createDirectories(target.getParent());

            // Write to a unique temporary name and move it into place. Two JVMs starting at once
            // would otherwise each write the same path, and one could execute the half-written
            // file the other was still copying. A move within one directory is atomic on every
            // supported platform.
            Path temp = Files.createTempFile(target.getParent(), "queryforge-", ".tmp");
            try (OutputStream out = Files.newOutputStream(temp)) {
                out.write(bytes);
            }
            ensureExecutable(temp);
            try {
                Files.move(temp, target, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
            } catch (IOException atomicUnsupported) {
                // Some filesystems (notably certain network mounts) reject ATOMIC_MOVE. A plain
                // replace is still correct here because the name is content-addressed: whatever
                // is already there has the same bytes.
                Files.move(temp, target, StandardCopyOption.REPLACE_EXISTING);
            }
            ensureExecutable(target);
            return target;

        } catch (IOException e) {
            throw new BinaryNotFoundException(
                    "Could not extract the bundled QueryForge executable: " + e.getMessage(), e);
        }
    }

    /**
     * Returns the on-disk location for an extracted binary.
     *
     * <p>The name carries a short hash of the contents, so different engine versions coexist and
     * an upgrade cannot be defeated by a stale file with the right name.
     */
    private static Path cacheLocation(String tag, byte[] bytes) {
        String custom = override(CACHE_DIR_PROPERTY, CACHE_DIR_ENV_VAR);
        Path root = custom != null
                ? Paths.get(custom)
                : Paths.get(System.getProperty("java.io.tmpdir"), "queryforge-bin");

        String suffix = platformTag().startsWith("windows") ? ".exe" : "";
        return root.resolve("queryforge-" + tag + "-" + shortHash(bytes) + suffix);
    }

    private static String shortHash(byte[] bytes) {
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256").digest(bytes);
            StringBuilder sb = new StringBuilder();
            for (int i = 0; i < 8; i++) {
                sb.append(String.format("%02x", digest[i]));
            }
            return sb.toString();
        } catch (Exception e) {
            // SHA-256 is mandated by the JDK, so this cannot happen; degrade to a length-based
            // name rather than failing a query over a hash.
            return "len" + bytes.length;
        }
    }

    private static void ensureExecutable(Path path) throws IOException {
        // On Windows the executable bit does not exist and setExecutable is a no-op, so this is
        // simply skipped rather than special-cased.
        if (!path.toFile().setExecutable(true, true) && !Files.isExecutable(path)) {
            throw new IOException("could not mark " + path + " executable");
        }
    }

    private static byte[] readAll(InputStream in) throws IOException {
        java.io.ByteArrayOutputStream buffer = new java.io.ByteArrayOutputStream(16 * 1024 * 1024);
        byte[] chunk = new byte[64 * 1024];
        int read;
        while ((read = in.read(chunk)) != -1) {
            buffer.write(chunk, 0, read);
        }
        return buffer.toByteArray();
    }

    /** Clears the memoized path. Exists for tests that repoint the override. */
    static void resetCache() {
        synchronized (BinaryResolver.class) {
            cached = null;
        }
    }
}
