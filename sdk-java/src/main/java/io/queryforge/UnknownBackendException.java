package io.queryforge;

import java.util.List;

/**
 * No generator is registered for the requested backend. The message lists the ones that are.
 */
public class UnknownBackendException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    UnknownBackendException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
