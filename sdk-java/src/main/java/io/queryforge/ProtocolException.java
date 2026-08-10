package io.queryforge;

/**
 * The executable produced something this SDK cannot interpret.
 *
 * <p>Reaching this means the engine crashed, wrote non-JSON to stdout, or speaks a protocol
 * version this SDK was not built against. It is the one error class that points at a broken
 * installation rather than a bad input.
 */
public class ProtocolException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    ProtocolException(String message) {
        super(message, "PROTOCOL_ERROR");
    }

    ProtocolException(String message, Throwable cause) {
        super(message, "PROTOCOL_ERROR", cause);
    }
}
