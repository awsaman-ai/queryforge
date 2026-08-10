package io.queryforge;

/**
 * No QueryForge executable could be found for this platform.
 *
 * <p>Thrown before any request is sent. The message names the platform that was detected, the
 * locations that were searched, and how to override the search with {@code QUERYFORGE_BINARY}.
 */
public class BinaryNotFoundException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    BinaryNotFoundException(String message) {
        super(message, "BINARY_NOT_FOUND");
    }

    BinaryNotFoundException(String message, Throwable cause) {
        super(message, "BINARY_NOT_FOUND", cause);
    }
}
