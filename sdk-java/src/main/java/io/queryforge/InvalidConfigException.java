package io.queryforge;

import java.util.List;

/**
 * The config did not parse, or broke one of the engine's structural rules.
 */
public class InvalidConfigException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    InvalidConfigException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
