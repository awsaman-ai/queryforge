package io.queryforge;

import java.util.List;

/**
 * The model was never reached: network failure, a missing or rejected API key, or a rate limit.
 *
 * <p>Check that the environment variable named by the config's {@code apiKeyEnv} is exported.
 */
public class ModelTransportException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    ModelTransportException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
