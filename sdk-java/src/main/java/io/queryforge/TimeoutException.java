package io.queryforge;

import java.util.List;

/**
 * The operation exceeded its deadline.
 *
 * <p>Named for the protocol's {@code TIMEOUT} code. It is unrelated to
 * {@code java.util.concurrent.TimeoutException}, which is checked.
 */
public class TimeoutException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    TimeoutException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
