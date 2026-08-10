package io.queryforge;

import java.util.List;

/**
 * A valid AST could not be compiled to the requested backend.
 */
public class GenerateException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    GenerateException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
